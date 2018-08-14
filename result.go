package mesg

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/mesg-foundation/core/api/core"
)

// Result is MESG result event.
type Result struct {
	TaskKey     string
	OutputKey   string
	data        string
	executionID string
}

// Data decodes result data into out.
func (e *Result) Data(out interface{}) error {
	return json.Unmarshal([]byte(e.data), out)
}

// ResultEmitter is a MESG result event listener.
type ResultEmitter struct {
	app *Application

	// resultTask is the actual event to listen for.
	resultTask string

	//resultServiceID is the service id of where result is emitted.
	resultServiceID string

	// outputKey is the output key to listen for.
	outputKey string

	// taskKey is the actual task that will be executed.
	taskKey string

	// taskServiceID is the service id of target task.
	taskServiceID string

	// filterFuncs holds funcs that returns boolean values to decide
	// if the task should be executed or not.
	filterFuncs []func(*Result) bool

	// mapFunc is a func that returns input data of task.
	mapFunc func(*Result) Data

	// m protects emitter configuration.
	m sync.Mutex

	gracefulWait *sync.WaitGroup
}

// ResultCondition is the condition configurator for filtering results.
type ResultCondition func(*ResultEmitter)

// TaskKeyCondition returns a new option to filter results by task name.
// Default is all(*).
func TaskKeyCondition(task string) ResultCondition {
	return func(l *ResultEmitter) {
		l.resultTask = task
	}
}

// OutputKeyCondition returns a new option to filter results by output key name.
// Default is all(*).
func OutputKeyCondition(key string) ResultCondition {
	return func(l *ResultEmitter) {
		l.outputKey = key
	}
}

// WhenResult creates a ResultEmitter for serviceID.
func (a *Application) WhenResult(serviceID string, conditions ...ResultCondition) *ResultEmitter {
	e := &ResultEmitter{
		app:             a,
		resultServiceID: serviceID,
		resultTask:      "*",
		outputKey:       "*",
		gracefulWait:    &sync.WaitGroup{},
	}
	for _, condition := range conditions {
		condition(e)
	}
	return e
}

// Filter sets filter funcs that will be executed to decide to execute the
// task or not.
// It's possible to add multiple filters by calling Filter multiple times.
// Other filter funcs and the task execution will no proceed if a filter
// func returns false.
func (e *ResultEmitter) Filter(fn func(*Result) bool) *ResultEmitter {
	e.m.Lock()
	defer e.m.Unlock()
	e.filterFuncs = append(e.filterFuncs, fn)
	return e
}

// Map sets the returned data as the input data of task.
// You can dynamically produce input values for task over result data.
func (e *ResultEmitter) Map(fn func(*Result) Data) Executor {
	e.m.Lock()
	defer e.m.Unlock()
	e.mapFunc = fn
	return e
}

// Execute starts for listening events and executes task for serviceID with the
// output data of result or return value of Map if all Filter funcs returned as true.
func (e *ResultEmitter) Execute(serviceID, task string) (*Listener, error) {
	e.taskServiceID = serviceID
	e.taskKey = task
	listener := newListener(e.app, e.gracefulWait)
	if err := e.app.startServices(e.resultServiceID, serviceID); err != nil {
		return nil, err
	}
	cancel, err := e.listen(listener)
	if err != nil {
		return nil, err
	}
	listener.cancel = cancel
	e.app.addListener(listener)
	return listener, nil
}

// listen starts listening for results.
func (e *ResultEmitter) listen(listener *Listener) (context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := e.app.client.ListenResult(ctx, &core.ListenResultRequest{
		ServiceID:    e.resultServiceID,
		TaskFilter:   e.resultTask,
		OutputFilter: e.outputKey,
	})
	if err != nil {
		return cancel, err
	}
	go e.readStream(listener, resp)
	return cancel, nil
}

// readStream reads listen result stream.
func (e *ResultEmitter) readStream(listener *Listener, resp core.Core_ListenResultClient) {
	for {
		e.gracefulWait.Add(1)
		data, err := resp.Recv()
		if err != nil {
			e.gracefulWait.Done()
			listener.sendError(err)
			return
		}
		result := &Result{
			TaskKey:   data.TaskKey,
			OutputKey: data.OutputKey,
			data:      data.OutputData,
		}
		go e.execute(listener, result)
	}
}

// execute executes the task with data returned from Map if all filters
// are met.
func (e *ResultEmitter) execute(listener *Listener, result *Result) {
	defer e.gracefulWait.Done()

	for _, filterFunc := range e.filterFuncs {
		if !filterFunc(result) {
			return
		}
	}

	var data Data
	if e.mapFunc != nil {
		data = e.mapFunc(result)
	} else if err := result.Data(&data); err != nil {
		e.app.log.Println(errDecodingResultData{err})
		return
	}

	if _, err := e.app.execute(e.taskServiceID, e.taskKey, data); err != nil {
		e.app.log.Println(executionError{e.taskKey, err})
	}
}

type errDecodingResultData struct {
	err error
}

func (e errDecodingResultData) Error() string {
	return fmt.Sprintf("cannot decode result data err: %s", e.err)
}
