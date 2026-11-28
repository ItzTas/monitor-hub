package monitorsettings

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ItzTas/monitor-hub/sdk-go/settings"
	"github.com/ItzTas/monitor-hub/src/internal/monitorconfig"
)

type NameInput struct {
	Name string `path:"name" doc:"The monitor's name."`
}

type ReadOutput struct {
	Body Settings
}

type Settings struct {
	Monitor     string         `json:"monitor"`
	Declared    bool           `json:"declared" doc:"Whether the monitor ships a settings.json version 2 the hub reads."`
	Revision    int64          `json:"revision" doc:"Send it back in a save to refuse it if someone saved in between. 0 before the first save."`
	Declaration *Declaration   `json:"declaration"`
	Document    map[string]any `json:"document" doc:"The values the monitor receives, without the secret fields and the fields it does not let you change."`
	Fields      []FieldState   `json:"fields"`
	Secrets     []SecretState  `json:"secrets"`
	Ignored     []Ignored      `json:"ignored"`
	Variables   []Variable     `json:"variables"`
	Problems    []Problem      `json:"problems"`
}

type Declaration struct {
	Fields []Field `json:"fields"`
}

type Field struct {
	Name     string          `json:"name"`
	Label    string          `json:"label"`
	Help     string          `json:"help,omitempty"`
	Type     string          `json:"type" enum:"string,integer,number,boolean,duration,choice,object,list"`
	Required bool            `json:"required"`
	Secret   bool            `json:"secret"`
	Editable bool            `json:"editable"`
	Reason   string          `json:"reason,omitempty"`
	Group    string          `json:"group,omitempty"`
	Env      string          `json:"env,omitempty"`
	Options  []string        `json:"options,omitempty"`
	Default  json.RawMessage `json:"default,omitempty"`
	Fields   []Field         `json:"fields,omitempty"`
	Items    *Item           `json:"items,omitempty"`
}

type Item struct {
	Type    string   `json:"type" enum:"string,integer,number,boolean,duration,choice,object,list"`
	Options []string `json:"options,omitempty"`
	Fields  []Field  `json:"fields,omitempty"`
}

type FieldState struct {
	Name   string `json:"name"`
	Source string `json:"source" enum:"page,env,default,unset"`
	Saved  bool   `json:"saved"`
	EnvSet bool   `json:"env_set"`
}

type SecretState struct {
	Path   string `json:"path"`
	Set    bool   `json:"set"`
	Saved  bool   `json:"saved"`
	EnvSet bool   `json:"env_set"`
}

type Ignored struct {
	Path   string          `json:"path"`
	Value  json.RawMessage `json:"value" doc:"null when the value is or was secret."`
	Secret bool            `json:"secret"`
	Reason string          `json:"reason" enum:"undeclared,type,not_editable,no_declaration,replaced"`
}

type Variable struct {
	Name   string  `json:"name"`
	Secret bool    `json:"secret"`
	Source string  `json:"source" enum:"page,env"`
	Value  *string `json:"value" doc:"null when the variable is secret."`
	Saved  bool    `json:"saved"`
	EnvSet bool    `json:"env_set"`
}

type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type SaveInput struct {
	Name string `path:"name" doc:"The monitor's name."`
	Body SaveBody
}

type SaveBody struct {
	Document      map[string]json.RawMessage `json:"document,omitempty" doc:"A value per top-level field; null goes back to the .env text or the default."`
	Secrets       map[string]*string         `json:"secrets,omitempty" doc:"A text per secret path; null goes back to the .env text or the default."`
	Variables     *VariablesChange           `json:"variables,omitempty"`
	DeleteIgnored []string                   `json:"delete_ignored,omitempty" doc:"Paths of ignored values to delete."`
	Revision      *int64                     `json:"revision,omitempty" doc:"The revision read before; a different one refuses the save with 409."`
}

type VariablesChange struct {
	Set    map[string]string `json:"set,omitempty"`
	Unset  []string          `json:"unset,omitempty"`
	Secret []string          `json:"secret,omitempty" doc:"Names in set to keep as secret."`
}

type SaveOutput struct {
	Status int
	Body   SaveResult
}

type SaveResult struct {
	Changed   []string `json:"changed" doc:"What the save changed: field names, secret paths, env:<NAME> and ignored:<path>."`
	Restarted bool     `json:"restarted"`
	Request   uint64   `json:"request" doc:"The restart to follow in the progress, 0 without one."`
}

type ProgressOutput struct {
	Body Progress
}

type Progress struct {
	Request      uint64     `json:"request"`
	State        string     `json:"state" enum:"none,restarting,up,failed,no_answer"`
	RequestedAt  *time.Time `json:"requested_at"`
	Phase        string     `json:"phase" enum:"not_started,starting,running,failed,exited"`
	MonitorState string     `json:"monitor_state" doc:"idle, checking, paused, failing or unknown when the state is up, empty otherwise."`
	Message      string     `json:"message"`
	Reason       string     `json:"reason"`
}

// ProblemsResponse is the 422 of a save the hub or the monitor refused.
type ProblemsResponse struct {
	Problems []Problem `json:"problems"`
}

func (r *ProblemsResponse) Error() string {
	return "the values have problems"
}

func (r *ProblemsResponse) GetStatus() int {
	return http.StatusUnprocessableEntity
}

// settingsOf copies the view field by field: the response types are the contract, not the view's.
func settingsOf(view monitorconfig.View) Settings {
	out := Settings{
		Monitor:   view.Monitor,
		Declared:  view.Declared,
		Revision:  view.Revision,
		Document:  view.Document,
		Fields:    make([]FieldState, 0, len(view.Fields)),
		Secrets:   make([]SecretState, 0, len(view.Secrets)),
		Ignored:   make([]Ignored, 0, len(view.Ignored)),
		Variables: make([]Variable, 0, len(view.Variables)),
		Problems:  problemsOf(view.Problems),
	}
	if out.Document == nil {
		out.Document = map[string]any{}
	}
	if view.Declaration != nil {
		out.Declaration = &Declaration{Fields: orEmpty(fieldsOf(view.Declaration.Fields))}
	}
	for _, f := range view.Fields {
		out.Fields = append(out.Fields, FieldState{Name: f.Name, Source: string(f.Source), Saved: f.Saved, EnvSet: f.EnvSet})
	}
	for _, s := range view.Secrets {
		out.Secrets = append(out.Secrets, SecretState{Path: s.Path, Set: s.Set, Saved: s.Saved, EnvSet: s.EnvSet})
	}
	for _, i := range view.Ignored {
		out.Ignored = append(out.Ignored, Ignored{Path: i.Path, Value: i.Value, Secret: i.Secret, Reason: string(i.Reason)})
	}
	for _, v := range view.Variables {
		out.Variables = append(out.Variables, Variable{Name: v.Name, Secret: v.Secret, Source: string(v.Source), Value: v.Value, Saved: v.Saved, EnvSet: v.EnvSet})
	}
	return out
}

func fieldsOf(fields []settings.Field) []Field {
	if len(fields) == 0 {
		return nil
	}
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		field := Field{
			Name:     f.Name,
			Label:    f.Label,
			Help:     f.Help,
			Type:     string(f.Type),
			Required: f.Required,
			Secret:   f.Secret,
			Editable: f.Editable,
			Reason:   f.Reason,
			Group:    f.Group,
			Env:      f.Env,
			Options:  f.Options,
			Default:  f.Default,
			Fields:   fieldsOf(f.Fields),
		}
		if f.Items != nil {
			field.Items = &Item{Type: string(f.Items.Type), Options: f.Items.Options, Fields: fieldsOf(f.Items.Fields)}
		}
		out = append(out, field)
	}
	return out
}

func problemsOf(problems []monitorconfig.Problem) []Problem {
	out := make([]Problem, 0, len(problems))
	for _, p := range problems {
		out = append(out, Problem{Path: p.Path, Message: p.Message})
	}
	return out
}
