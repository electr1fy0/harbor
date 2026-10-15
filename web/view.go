package web

import (
	"strconv"
	"strings"
)

//go:generate go run github.com/a-h/templ/cmd/templ@v0.3.960 generate -f dashboard.templ

// Page is the complete, typed view rendered by the daemon. Live updates replace
// sections only, so refreshes never overwrite a user's in-progress form.
type Page struct {
	Title, Description, Name, Path, Tab, Error, CSS, JS string
	Status                                              int
	Public                                              bool
	Nav, Tabs                                           []Link
	Sections                                            []Section
	Forms                                               []Form
	HeaderLinks                                         []Link
	Filter                                              ProjectFilter
	ShowProjectFilters                                  bool
}
type ProjectFilter struct {
	Query               string
	Favorites, Archived bool
}
type Link struct {
	Label, URL string
	Active     bool
}
type Metric struct{ Label, Value, Detail string }
type Section struct {
	ID, Title, Description, Text, Log string
	Metrics                           []Metric
	Table                             Table
	Actions                           []Form
	Links                             []Link
	Chart                             []Point
	Compact                           bool
}
type Table struct {
	Headers    []string
	Rows       []Row
	Empty      string
	EmptyTitle string
	EmptyLinks []Link
}
type Row struct {
	Cells   []Cell
	Actions []Form
}
type Cell struct{ Text, URL, Badge, Detail string }
type Point struct {
	X, Y  float64
	Label string
}
type Form struct {
	ID, Title, Description, Function, Action, Submit, Confirm, Redirect string
	Fields                                                              []Field
	Inline, Inspect, Disabled                                           bool
	Collapsible, Danger                                                 bool
	Unavailable, Success                                                string
	Links                                                               []Link
}
type Field struct {
	Name, Label, Type, Value, Placeholder, Help, Min, Max, Step string
	Options                                                     []Option
	Required, Checked, OmitEmpty                                bool
	Group                                                       string
}
type Option struct{ Value, Label string }
type FieldGroup struct {
	Label  string
	Fields []Field
}

func (f Form) PrimaryFields() []Field {
	var out []Field
	for _, field := range f.Fields {
		if field.Group == "" {
			out = append(out, field)
		}
	}
	return out
}
func (f Form) FieldGroups() []FieldGroup {
	var groups []FieldGroup
	for _, field := range f.Fields {
		if field.Group == "" {
			continue
		}
		index := -1
		for i := range groups {
			if groups[i].Label == field.Group {
				index = i
				break
			}
		}
		if index == -1 {
			groups = append(groups, FieldGroup{Label: field.Group})
			index = len(groups) - 1
		}
		groups[index].Fields = append(groups[index].Fields, field)
	}
	return groups
}
func (f Field) Wide() bool {
	return f.Type == "textarea" || f.Type == "env" || f.Type == "list" || f.Help != ""
}

func StatusLabel(s string) string {
	switch s {
	case "OOM_KILLED":
		return "Out of memory"
	case "BUILD_FAILED":
		return "Build failed"
	case "START_FAILED":
		return "Start failed"
	case "HEALTH_CHECKING":
		return "Checking health"
	}
	if s == "" {
		return "—"
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(strings.ReplaceAll(s[1:], "_", " "))
}

func (p Page) Login() bool { return p.Path == "/login" }
func (f Field) InputType() string {
	switch f.Type {
	case "number", "password", "url", "hidden", "checkbox":
		return f.Type
	}
	return "text"
}
func (p Page) TitleText() string            { return p.Title + " · Harbor" }
func (f Form) ResultID() string             { return f.ID + "-result" }
func (f Field) TextID(formID string) string { return formID + "-" + f.Name }
func StatusClass(s string) string {
	switch s {
	case "RUNNING", "SUCCEEDED", "verified", "healthy", "READY":
		return "good"
	case "FAILED", "BUILD_FAILED", "START_FAILED", "UNHEALTHY", "CRASHED", "OOM_KILLED", "failed":
		return "bad"
	default:
		return "neutral"
	}
}
func Number(i int) string       { return strconv.Itoa(i) }
func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
func fmtBool(v bool) string     { return strconv.FormatBool(v) }
func autocomplete(typ string) string {
	if typ == "password" {
		return "off"
	}
	return "on"
}

func current(active bool) string {
	if active {
		return "page"
	}
	return "false"
}
