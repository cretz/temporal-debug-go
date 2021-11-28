package tracer

import "go.temporal.io/api/enums/v1"

type Result struct {
	Events []Event `json:"events"`
}

type Event struct {
	// Only one of these is present
	Server *EventServer `json:"server,omitempty"`
	Client *EventClient `json:"client,omitempty"`
	Code   *EventCode   `json:"code,omitempty"`
}

type EventServer struct {
	ID   int64           `json:"eventId"`
	Type EventServerType `json:"eventType"`
}

type EventServerType enums.EventType

func (e *EventServerType) UnmarshalText(text []byte) error {
	*e = EventServerType(enums.EventType_value[string(text)])
	return nil
}

func (e EventServerType) MarshalText() ([]byte, error) {
	return []byte(e.String()), nil
}

func (e EventServerType) String() string {
	return enums.EventType(e).String()
}

type EventClient struct {
	Commands []EventClientCommandType `json:"commands,omitempty"`
}

type EventClientCommandType enums.CommandType

func (e *EventClientCommandType) UnmarshalText(text []byte) error {
	*e = EventClientCommandType(enums.CommandType_value[string(text)])
	return nil
}

func (e EventClientCommandType) MarshalText() ([]byte, error) {
	return []byte(e.String()), nil
}

func (e EventClientCommandType) String() string {
	return enums.CommandType(e).String()
}

type EventCode struct {
	Package   string `json:"package,omitempty"`
	File      string `json:"file,omitempty"`
	Line      int    `json:"line,omitempty"`
	Coroutine string `json:"coroutine,omitempty"`
	// TODO(cretz): Locals
	// LocalsUpdated []api.Variable `json:"locals_updated,omitempty"`
}
