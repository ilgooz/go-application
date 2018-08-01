package mesg

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/mesg-foundation/core/api/core"
)

// Event is a MESG event.
type Event struct {
	Key  string
	data string
}

// Data decodes event data into out.
func (e *Event) Data(out interface{}) error {
	return json.Unmarshal([]byte(e.data), out)
}

// EventEmitter is a MESG event emitter.
type EventEmitter struct {
	app *Application

	// event is the actual event to listen for.
	event string

	//eventServiceID is the service id of where event is emitted.
	eventServiceID string

	// task is the actual task that will be executed.
	task string

	// taskServiceID is the service id of target task.
	taskServiceID string

	// filterFuncs holds funcs that returns boolean values to decide
	// if the task should be executed or not.
	filterFuncs []func(*Event) bool

	// provideFunc is a func that returns input data of task.
	mapFunc func(*Event) Data

	// m protects emitter configuration.
	m sync.RWMutex

	// cancel cancels listening for upcoming events.
	cancel context.CancelFunc
}

// EventOption is the configuration func of EventListener.
type EventOption func(*EventEmitter)

// EventFilterOption returns a new option to filter events by name.
// Default is all(*).
func EventFilterOption(event string) EventOption {
	return func(l *EventEmitter) {
		l.event = event
	}
}

// WhenEvent creates an EventListener for serviceID.
func (a *Application) WhenEvent(serviceID string, options ...EventOption) *EventEmitter {
	e := &EventEmitter{
		app:            a,
		eventServiceID: serviceID,
		event:          "*",
	}
	for _, option := range options {
		option(e)
	}
	return e
}

// Filter expects the returned value to be true to do task execution.
func (e *EventEmitter) Filter(fn func(*Event) bool) *EventEmitter {
	e.m.Lock()
	defer e.m.Unlock()
	e.filterFuncs = append(e.filterFuncs, fn)
	return e
}

// Data is piped as the input data to task.
type Data interface{}

// Map sets the returned data as the input data of task.
// You can dynamically produce input values for task over event data.
func (e *EventEmitter) Map(fn func(*Event) Data) *Executor {
	e.m.Lock()
	defer e.m.Unlock()
	e.mapFunc = fn
	return newEventEmitterExecutor(e)
}

// Execute executes task on serviceID.
func (e *EventEmitter) start(serviceID, task string) (*Stream, error) {
	e.taskServiceID = serviceID
	e.task = task
	stream := &Stream{
		Executions: make(chan *Execution, 0),
		Err:        make(chan error, 0),
	}
	if err := e.app.startServices(e.eventServiceID, serviceID); err != nil {
		return nil, err
	}
	cancel, err := e.listen(stream)
	if err != nil {
		return nil, err
	}
	stream.cancel = cancel
	return stream, nil
}

// Listen starts listening for events.
func (e *EventEmitter) listen(stream *Stream) (context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := e.app.client.ListenEvent(ctx, &core.ListenEventRequest{
		ServiceID:   e.eventServiceID,
		EventFilter: e.event,
	})
	if err != nil {
		return cancel, err
	}
	go e.readStream(stream, resp)
	return cancel, nil
}

func (e *EventEmitter) readStream(stream *Stream, resp core.Core_ListenEventClient) {
	for {
		data, err := resp.Recv()
		if err != nil {
			stream.Err <- err
			return
		}
		event := &Event{
			Key:  data.EventKey,
			data: data.EventData,
		}
		go e.execute(stream, event)
	}
}

func (e *EventEmitter) execute(stream *Stream, event *Event) {
	e.m.RLock()
	for _, filterFunc := range e.filterFuncs {
		if !filterFunc(event) {
			e.m.RUnlock()
			return
		}
	}
	e.m.RUnlock()

	var data Data
	if e.mapFunc != nil {
		data = e.mapFunc(event)
	} else if err := event.Data(&data); err != nil {
		stream.Executions <- &Execution{
			Err: err,
		}
		return
	}

	executionID, err := e.app.execute(e.taskServiceID, e.task, data)
	stream.Executions <- &Execution{
		ID:  executionID,
		Err: err,
	}
}