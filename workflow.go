package alfred

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/template"
)

// ModKey is a modifier key (e.g., cmd, ctrl, alt)
type ModKey string

// ModKey constants
const (
	ModCmd   ModKey = "cmd"
	ModShift ModKey = "shift"
	ModAlt   ModKey = "alt"
	ModCtrl  ModKey = "ctrl"
)

// ModeType describes the workflow's current mode
type ModeType string

// ModeType constants
const (
	ModeDo   ModeType = "do"
	ModeTell ModeType = "tell"
	ModeBack ModeType = "back"
)

// CommandDef describes a workflow command
type CommandDef struct {
	Keyword     string
	Description string
	Mods        map[ModKey]ItemMod
	IsEnabled   bool
	Arg         *ItemArg
}

// KeywordItem creates a new Item for a command definition
func (c *CommandDef) KeywordItem() (item Item) {
	item.Title = c.Keyword
	item.Autocomplete = c.Keyword
	item.Subtitle = c.Description

	if c.Arg != nil {
		item.Arg = c.Arg
	} else {
		item.Arg = &ItemArg{Keyword: c.Keyword}
	}

	if c.Mods != nil {
		for key, mod := range c.Mods {
			item.AddMod(key, mod)
		}
	}

	return
}

// Command is a Filter or Action
type Command interface {
	About() CommandDef
}

// Filter is a Command that creates a filtered list of items
type Filter interface {
	Command
	Items(arg, data string) ([]Item, error)
}

// Action is Command that does something
type Action interface {
	Command
	Do(arg, data string) (string, error)
}

// Workflow represents an Alfred workflow
type Workflow struct {
	name     string
	bundleID string
	cacheDir string
	dataDir  string
}

// OpenWorkflow returns a Workflow for a given directory. If the createDirs
// option is true, cache and data directories will be created for the workflow.
func OpenWorkflow(workflowDir string, createDirs bool) (w Workflow, err error) {
	bundleID := os.Getenv("alfred_workflow_bundleid")
	name := os.Getenv("alfred_workflow_name")
	cacheDir := os.Getenv("alfred_workflow_cache")
	dataDir := os.Getenv("alfred_workflow_data")

	if createDirs {
		if err = os.MkdirAll(cacheDir, 0755); err != nil {
			return
		}
		if err = os.MkdirAll(dataDir, 0755); err != nil {
			return
		}
	}

	w = Workflow{
		name:     name,
		bundleID: bundleID,
		cacheDir: cacheDir,
		dataDir:  dataDir,
	}

	return
}

// Run runs a workflow.
//
// A Workflow understands the following command line formats
//
//  $ ./workflow (arg|data)
//  $ ./workflow arg data
//  $ ./workflow -final data
//
// Run takes one parameter: a list of Commands. Commands may be Filters or
// Actions. Filters are commands that generate lists of items, while Actions
// are commands that take an action.
//
// When the mode is "tell"...
//   * ...and a keyword was specified in the incoming data, the Filter matching
//     that keyword (if there is one) is called to generate items
//   * ...and no keyword was specified in the incoming data, items are generated
//     for:
//     * any Filter with a fuzzy-matching keyword
//     * any Action with a fuzzy-matching keyword and an Arg in its CommandDef
//
func (w *Workflow) Run(commands []Command) {
	var mode ModeType
	var final bool
	var arg string
	var data workflowData
	var keyword string
	var prefix string
	var err error

	flag.BoolVar(&final, "final", false, "If true, act as the final workflow "+
		"stage")
	flag.Parse()

	args := flag.Args()

	if len(args) == 1 {
		// If there's only 1 arg, try to decode it as a workflow data object,
		// otherwise it'll be treated as the arg
		if err := json.Unmarshal([]byte(args[0]), &data); err != nil {
			dlog.Printf("Couldn't parse first arg as data: %v", err)
			arg = args[0]
		}
	} else if len(args) > 1 {
		// If there are 2 args, the second must be a workflow data object. Use
		// the first as `arg` even if the data object contains an Arg value.
		arg = args[0]
		if args[1] != "" {
			if err = json.Unmarshal([]byte(args[1]), &data); err != nil {
				dlog.Printf("Couldn't parse second arg as data: %v", err)
			}
		}
	} else {
		err = fmt.Errorf("More than 2 args were provided; only 2 are accepted")
	}

	if err == nil {
		// If this is the final step in the workflow, the data should be
		// actionable
		if final {
			if data.Mode == "" {
				data.Mode = ModeTell
			}

			if data.Mode == ModeBack {
				dlog.Printf("going back")
				json.Unmarshal([]byte(data.Data), &data)
			}

			if data.Mode == ModeBack || data.Mode == ModeTell {
				var block blockConfig
				block.AlfredWorkflow.Variables.Data = Stringify(&data)

				out, err := RunScript(fmt.Sprintf(`tell application "`+
					appName+`" to run trigger "start" in workflow "`+
					w.bundleID+`" with argument %s`,
					strconv.Quote(Stringify(&block))))
				if err != nil {
					dlog.Printf("Error running loopback script: %v", err)
				} else {
					dlog.Println(out)
				}

				return
			}
		}

		// If don't have a mode, assume 'tell'
		if data.Mode == "" {
			data.Mode = ModeTell
		}

		keyword = data.Keyword
		dlog.Printf("set keyword to '%s'", keyword)

		// If the keyword wasn't specified in the incoming data, parse it out
		// of the argument. The keyword part of the argument will become the
		// prefix, and the remainder will be passed to Items or Do as the arg
		if keyword == "" {
			cmd, rest := SplitCmd(arg)
			keyword = cmd

			// Use the keyword as the prefix. If the arg has more characters
			// than the keyword, there must be a space after the keyword.
			prefix = keyword
			if len(arg) > len(keyword) {
				prefix += " "
			}

			// The rest of the original arg is the new arg
			arg = rest
		} else {
			arg = strings.Trim(arg, " ")
		}
	}

	switch data.Mode {
	case "tell":
		var items Items

		if err == nil {
			dlog.Printf("tell: data=%#v, arg='%s'", data, arg)

			for _, c := range commands {
				def := c.About()

				// Skip disabled commands
				if !def.IsEnabled {
					dlog.Printf("Skipping disabled command '%s'", def.Keyword)
					continue
				}

				if data.Keyword != "" {
					if f, ok := c.(Filter); ok && def.Keyword == data.Keyword {
						dlog.Printf("Adding items for '%s'", def.Keyword)
						var filterItems []Item
						if filterItems, err = f.Items(arg, data.Data); err == nil {
							for _, i := range filterItems {
								items = append(items, i)

								// Add the prefix to Autocomplete strings
								if i.Autocomplete != "" {
									i.Autocomplete = prefix + i.Autocomplete
								}
							}
						}
					}
				} else if FuzzyMatches(def.Keyword, keyword) {
					if _, ok := c.(Filter); ok || def.Arg != nil {
						dlog.Printf("Adding menu item for '%s'", def.Keyword)
						items = append(items, def.KeywordItem())
					}
				}
			}

			if arg != "" {
				FuzzySort(items, arg)
			}
		}

		if err != nil {
			dlog.Printf("Error: %s", err)
			items = append(items, Item{Title: fmt.Sprintf("Error: %s", err)})
		} else if len(items) == 0 {
			items = append(items, Item{Title: fmt.Sprintf("No results")})
		}

		w.SendToAlfred(items, data)

	case "do":
		var output string

		// Note that in "do" mode only the "data" input is used

		if err == nil {
			// First, close the Alfred window
			// TODO: Could show an activity message instead
			if data.Mod == "" {
				RunScript(fmt.Sprintf(`tell application "System Events" to ` +
					`key code 53`))
			}

			var action Action

			for _, c := range commands {
				def := c.About()

				// Skip disabled commands
				if !def.IsEnabled {
					continue
				}

				if a, ok := c.(Action); ok {
					dlog.Printf("Checking if '%s' == '%s'", a.About().Keyword,
						keyword)
					if def.Keyword == keyword {
						action = a
						break
					}
				}
			}

			if action == nil {
				err = fmt.Errorf("No valid command in '%s'", arg)
			} else {
				output, err = action.Do(arg, data.Data)
			}
		}

		if err != nil {
			output = fmt.Sprintf("Error: %s", err)
		}

		if output != "" {
			fmt.Println(output)
		}

	default:
		fmt.Printf("Invalid mode: '%s'\n", mode)
	}
}

// AddPassword adds or updates a password in the macOS Keychain
func (w *Workflow) AddPassword(name, password string) (err error) {
	var out []byte
	out, err = exec.Command("security", "add-generic-password", "-w", "-g",
		"-a", w.bundleID, "-s", name, "-w", password, "-U").Output()
	if err != nil {
		dlog.Printf("Error adding password: %s", string(out))
	}
	return
}

// CacheDir returns the cache directory for a workflow.
func (w *Workflow) CacheDir() string {
	return w.cacheDir
}

// DataDir returns the data directory for a workflow.
func (w *Workflow) DataDir() string {
	return w.dataDir
}

// BundleID returns a workflow's bundle ID.
func (w *Workflow) BundleID() string {
	return w.bundleID
}

// GetConfirmation opens a confirmation dialog to ask the user to confirm
// something.
func (w *Workflow) GetConfirmation(prompt string, defaultYes bool) (confirmed bool, err error) {
	version := os.Getenv("alfred_short_version")
	type ScriptData struct {
		Version string
		Prompt  string
		Title   string
		Default string
	}

	script :=
		`tell application "Alfred {{.Version}}"
			  activate
			  set alfredPath to (path to application "Alfred {{.Version}}")
			  set alfredIcon to path to resource "appicon.icns" in bundle (alfredPath as alias)
			  display dialog "{{.Prompt}}" with title "{{.Title}}" buttons {"Yes", "No"} default button "{{.Default}}" with icon alfredIcon
		  end tell`

	data := ScriptData{version, prompt, w.name, "No"}
	if defaultYes {
		data.Default = "Yes"
	}

	var tmpl *template.Template
	tmpl, err = template.New("script").Parse(script)
	if err != nil {
		return
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return
	}

	script = buf.String()
	var response string
	response, err = RunScript(script)
	if err != nil {
		return
	}

	button, _ := parseDialogResponse(response)
	return button == "Yes", nil
}

// GetInput opens an input dialog to ask the user for some information.
func (w *Workflow) GetInput(prompt, defaultVal string, hideAnswer bool) (button, value string, err error) {
	script :=
		`tell application "Alfred {{.Version}}"
			  activate
			  set alfredPath to (path to application "Alfred {{.Version}}")
			  set alfredIcon to path to resource "appicon.icns" in bundle (alfredPath as alias)
			  display dialog "{{.Prompt}}:" with title "{{.Title}}" default answer "{{.Default}}" buttons {"Cancel", "Ok"} default button "Ok" with icon alfredIcon{{.Hidden}}
		  end tell`

	data := struct {
		Version string
		Prompt  string
		Title   string
		Default string
		Hidden  string
	}{os.Getenv("alfred_short_version"), prompt, w.name, defaultVal, ""}

	if hideAnswer {
		data.Hidden = " with hidden answer"
	}

	var tmpl *template.Template
	tmpl, err = template.New("script").Parse(script)
	if err != nil {
		return
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return
	}

	script = buf.String()
	var response string
	response, err = RunScript(script)
	dlog.Printf("got response: '%s'", response)
	if err != nil {
		if strings.Contains(response, "User canceled") {
			dlog.Printf("User canceled")
			return "Cancel", "", nil
		}
		return
	}

	button, value = parseDialogResponse(response)
	return
}

// GetPassword returns a workflow-specific password from the macOS Keychain
func (w *Workflow) GetPassword(name string) (pw string, err error) {
	var out []byte
	out, err = exec.Command("security", "find-generic-password", "-w", "-g",
		"-a", w.bundleID, "-s", name).Output()
	if err != nil {
		dlog.Printf("Error getting password: %s", string(out))
		return
	}
	pw = strings.TrimSpace(string(out))
	return
}

// SendToAlfred sends an array of items to Alfred. Currently this equates to
// outputting an Alfred JSON message on stdout.
func (w *Workflow) SendToAlfred(items Items, data workflowData) {
	for _, item := range items {
		item.data = data
	}
	out, _ := json.Marshal(items)
	fmt.Println(string(out))
}

// ShowMessage opens a message dialog to show the user a message.
func (w *Workflow) ShowMessage(message string) (err error) {
	script :=
		`tell application "Alfred {{.Version}}"
			  activate
			  set alfredPath to (path to application "Alfred {{.Version}}")
			  set alfredIcon to path to resource "appicon.icns" in bundle (alfredPath as alias)
			  display dialog "{{.Prompt}}" with title "{{.Title}}" buttons {"Ok"} default button "Ok" with icon alfredIcon
		  end tell`

	data := struct {
		Version string
		Prompt  string
		Title   string
	}{os.Getenv("alfred_short_version"), message, w.name}

	var tmpl *template.Template
	tmpl, err = template.New("script").Parse(script)
	if err != nil {
		return
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return
	}

	script = buf.String()
	_, err = RunScript(script)
	return
}

// support -------------------------------------------------------------------

// blockConfig is a struct used by Alfred to configure blocks
type blockConfig struct {
	AlfredWorkflow struct {
		Arg       string `json:"arg"`
		Variables struct {
			Data string `json:"data,omitempty"`
		} `json:"variables,omitempty"`
	} `json:"alfredworkflow"`
}

// workflowData describes the state of the workflow. It is used to communicate
// between workflow instances. All the elements of this structure should be
// primitives to allow easy copying.
type workflowData struct {
	Keyword string   `json:"keyword,omitempty"`
	Mode    ModeType `json:"mode,omitempty"`
	Mod     ModKey   `json:"mod,omitempty"`
	// Data is keyword-specific data
	Data string `json:"data,omitempty"`
}
