package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/satori/go.uuid"

	"github.com/lxc/lxd/shared"
)

var lock sync.Mutex
var operations map[string]*shared.Operation = make(map[string]*shared.Operation)

func createOperation(metadata shared.Jmap, resources map[string][]string, run func(id string) shared.OperationResult, cancel func(id string) error, ws shared.OperationWebsocket) (string, error) {
	id := uuid.NewV4().String()
	op := shared.Operation{}
	op.Id = id
	op.CreatedAt = time.Now()
	op.UpdatedAt = op.CreatedAt
	op.SetStatus(shared.Pending)

	if resources != nil {
		tmpResources := make(map[string][]string)
		for key, value := range resources {
			var values []string
			for _, c := range value {
				values = append(values, fmt.Sprintf("/%s/%s/%s", shared.APIVersion, key, c))
			}
			tmpResources[key] = values
		}
		op.Resources = tmpResources
	}

	md, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	op.Metadata = md

	op.MayCancel = ((run == nil && cancel == nil) || cancel != nil)

	op.Run = run
	op.Cancel = cancel
	op.Chan = make(chan bool, 1)
	op.Websocket = ws

	url := shared.OperationsURL(id)

	lock.Lock()
	operations[url] = &op
	lock.Unlock()
	return url, nil
}

func startOperation(id string) error {
	lock.Lock()
	op, ok := operations[id]
	if !ok {
		lock.Unlock()
		return fmt.Errorf("operation %s doesn't exist", id)
	}

	if op.Run != nil {
		go func(op *shared.Operation) {
			result := op.Run(id)

			shared.Debugf("Operation %s finished: %s", op.Run, result)

			lock.Lock()
			op.SetResult(result)
			lock.Unlock()
		}(op)
	}

	op.SetStatus(shared.Running)
	lock.Unlock()

	return nil
}

func updateOperation(id string, metadata map[string]string) error {
	op, ok := operations[id]
	if !ok {
		return fmt.Errorf("Operation doesn't exist")
	}

	md, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	op.Metadata = md

	return nil
}

func operationsGet(d *Daemon, r *http.Request) Response {
	var ops shared.Jmap

	recursion := d.isRecursionRequest(r)

	if recursion {
		ops = shared.Jmap{"pending": make([]*shared.Operation, 0, 0), "running": make([]*shared.Operation, 0, 0)}
	} else {
		ops = shared.Jmap{"pending": make([]string, 0, 0), "running": make([]string, 0, 0)}
	}

	lock.Lock()
	for k, v := range operations {
		switch v.StatusCode {
		case shared.Pending:
			if recursion {
				ops["pending"] = append(ops["pending"].([]*shared.Operation), operations[k])
			} else {
				ops["pending"] = append(ops["pending"].([]string), k)
			}
		case shared.Running:
			if recursion {
				ops["running"] = append(ops["running"].([]*shared.Operation), operations[k])
			} else {
				ops["running"] = append(ops["running"].([]string), k)
			}
		}
	}
	lock.Unlock()

	return SyncResponse(true, ops)
}

var operationsCmd = Command{name: "operations", get: operationsGet}

func operationGet(d *Daemon, r *http.Request) Response {
	id := shared.OperationsURL(mux.Vars(r)["id"])

	lock.Lock()
	defer lock.Unlock()
	op, ok := operations[id]
	if !ok {
		return NotFound
	}

	return SyncResponse(true, op)
}

func operationDelete(d *Daemon, r *http.Request) Response {
	lock.Lock()
	id := shared.OperationsURL(mux.Vars(r)["id"])
	op, ok := operations[id]
	if !ok {
		lock.Unlock()
		return NotFound
	}

	if op.Cancel == nil && op.Run != nil {
		lock.Unlock()
		return BadRequest(fmt.Errorf("Can't cancel %s!", id))
	}

	if op.StatusCode == shared.Cancelling || op.StatusCode.IsFinal() {
		/* the user has already requested a cancel, or the status is
		 * in a final state. */
		lock.Unlock()
		return EmptySyncResponse
	}

	if op.Cancel != nil {
		cancel := op.Cancel
		op.SetStatus(shared.Cancelling)
		lock.Unlock()

		err := cancel(id)

		lock.Lock()
		op.SetStatusByErr(err)
		lock.Unlock()

		if err != nil {
			return InternalError(err)
		}
	} else {
		op.SetStatus(shared.Cancelled)
		lock.Unlock()
	}

	return EmptySyncResponse
}

var operationCmd = Command{name: "operations/{id}", get: operationGet, delete: operationDelete}

func operationWaitGet(d *Daemon, r *http.Request) Response {
	targetStatus, err := shared.AtoiEmptyDefault(r.FormValue("status_code"), int(shared.Success))
	if err != nil {
		return InternalError(err)
	}

	timeout, err := shared.AtoiEmptyDefault(r.FormValue("timeout"), -1)
	if err != nil {
		return InternalError(err)
	}

	lock.Lock()
	id := shared.OperationsURL(mux.Vars(r)["id"])
	op, ok := operations[id]
	if !ok {
		lock.Unlock()
		return NotFound
	}

	status := op.StatusCode
	lock.Unlock()

	if int(status) != targetStatus && (status == shared.Pending || status == shared.Running) {

		if timeout >= 0 {
			select {
			case <-op.Chan:
				break
			case <-time.After(time.Duration(timeout) * time.Second):
				break
			}
		} else {
			<-op.Chan
		}
	}

	lock.Lock()
	defer lock.Unlock()
	return SyncResponse(true, op)
}

var operationWait = Command{name: "operations/{id}/wait", get: operationWaitGet}

type websocketServe struct {
	req    *http.Request
	secret string
	op     *shared.Operation
}

func (r *websocketServe) Render(w http.ResponseWriter) error {
	return r.op.Websocket.Connect(r.secret, r.req, w)
}

func operationWebsocketGet(d *Daemon, r *http.Request) Response {
	lock.Lock()
	defer lock.Unlock()
	id := shared.OperationsURL(mux.Vars(r)["id"])
	op, ok := operations[id]
	if !ok {
		return NotFound
	}

	if op.Websocket == nil {
		return BadRequest(fmt.Errorf("operation has no websocket protocol"))
	}

	secret := r.FormValue("secret")
	if secret == "" {
		return BadRequest(fmt.Errorf("missing secret"))
	}

	if op.StatusCode.IsFinal() {
		return BadRequest(fmt.Errorf("status is %s, can't connect", op.Status))
	}

	return &websocketServe{r, secret, op}
}

var operationWebsocket = Command{name: "operations/{id}/websocket", untrustedGet: true, get: operationWebsocketGet}
