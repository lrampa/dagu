package scheduler

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/yohamta/dagu/internal/config"
	"github.com/yohamta/dagu/internal/constants"
	"github.com/yohamta/dagu/internal/settings"
)

type SchedulerStatus int

const (
	SchedulerStatus_None SchedulerStatus = iota
	SchedulerStatus_Running
	SchedulerStatus_Error
	SchedulerStatus_Cancel
	SchedulerStatus_Success
	SchedulerStatus_Skipped_Unused
)

func (s SchedulerStatus) String() string {
	switch s {
	case SchedulerStatus_Running:
		return "running"
	case SchedulerStatus_Error:
		return "failed"
	case SchedulerStatus_Cancel:
		return "canceled"
	case SchedulerStatus_Success:
		return "finished"
	case SchedulerStatus_None:
		fallthrough
	default:
		return "not started"
	}
}

type Scheduler struct {
	*Config
	canceled  int32
	mu        sync.RWMutex
	pause     time.Duration
	lastError error
	handlers  map[string]*Node
}

type Config struct {
	LogDir        string
	MaxActiveRuns int
	Delay         time.Duration
	Dry           bool
	OnExit        *config.Step
	OnSuccess     *config.Step
	OnFailure     *config.Step
	OnCancel      *config.Step
}

func New(config *Config) *Scheduler {
	return &Scheduler{
		Config: config,
		pause:  100 * time.Millisecond,
	}
}

func (sc *Scheduler) Schedule(g *ExecutionGraph, done chan *Node) error {
	if err := sc.setup(); err != nil {
		return err
	}
	g.StartedAt = time.Now()

	defer func() {
		g.FinishedAt = time.Now()
	}()

	var wg = sync.WaitGroup{}

	for !sc.isFinished(g) {
		if sc.IsCanceled() {
			break
		}
		for _, node := range g.Nodes() {
			if node.ReadStatus() != NodeStatus_None {
				continue
			}
			if !isReady(g, node) {
				continue
			}
			if sc.IsCanceled() {
				break
			}
			if sc.MaxActiveRuns > 0 &&
				sc.runningCount(g) >= sc.MaxActiveRuns {
				continue
			}
			if len(node.Preconditions) > 0 {
				log.Printf("checking pre conditions for \"%s\"", node.Name)
				if err := config.EvalConditions(node.Preconditions); err != nil {
					log.Printf("%s", err.Error())
					node.updateStatus(NodeStatus_Skipped)
					node.Error = err
					continue
				}
			}
			wg.Add(1)

			log.Printf("start running: %s", node.Name)
			node.updateStatus(NodeStatus_Running)
			go func(node *Node) {
				defer func() {
					node.FinishedAt = time.Now()
					wg.Done()
				}()

				if !sc.Dry {
					node.setupLog(sc.LogDir)
					node.openLogFile()
					defer node.closeLogFile()
				}

				for !sc.IsCanceled() {
					var err error = nil
					if !sc.Dry {
						err = node.Execute()
					}
					if err != nil {
						handleError(node)
						switch node.ReadStatus() {
						case NodeStatus_None:
							// nothing to do
						case NodeStatus_Error:
							sc.lastError = err
						}
					}
					if node.ReadStatus() != NodeStatus_Cancel {
						node.incDoneCount()
					}
					if node.RepeatPolicy.Repeat {
						if err == nil || node.ContinueOn.Failure {
							if !sc.IsCanceled() {
								time.Sleep(node.RepeatPolicy.Interval)
								continue
							}
						}
					}
					if err != nil {
						if done != nil {
							done <- node
						}
						return
					}
					break
				}
				if node.ReadStatus() == NodeStatus_Running {
					node.updateStatus(NodeStatus_Success)
				}
				if done != nil {
					done <- node
				}
			}(node)

			time.Sleep(sc.Delay)
		}

		time.Sleep(sc.pause)
	}
	wg.Wait()

	handlers := []string{}
	switch sc.Status(g) {
	case SchedulerStatus_Success:
		handlers = append(handlers, constants.OnSuccess)
	case SchedulerStatus_Error:
		handlers = append(handlers, constants.OnFailure)
	case SchedulerStatus_Cancel:
		handlers = append(handlers, constants.OnCancel)
	}
	handlers = append(handlers, constants.OnExit)
	for _, h := range handlers {
		if n := sc.handlers[h]; n != nil {
			log.Println(fmt.Sprintf("%s started", n.Name))
			err := sc.runHandlerNode(n)
			if err != nil {
				sc.lastError = err
			}
			if done != nil {
				done <- n
			}
		}
	}
	return sc.lastError
}

func (sc *Scheduler) runHandlerNode(node *Node) error {
	defer func() {
		node.FinishedAt = time.Now()
	}()

	node.updateStatus(NodeStatus_Running)

	if !sc.Dry {
		node.setupLog(sc.LogDir)
		node.openLogFile()
		defer node.closeLogFile()
		err := node.Execute()
		if err != nil {
			node.updateStatus(NodeStatus_Error)
		} else {
			node.updateStatus(NodeStatus_Success)
		}
	} else {
		node.updateStatus(NodeStatus_Success)
	}

	return nil
}

func (sc *Scheduler) setup() (err error) {
	if sc.LogDir == "" {
		sc.LogDir, err = settings.Get(settings.CONFIG__LOGS_DIR)
		if err != nil {
			return
		}
	}
	if !sc.Dry {
		if err = os.MkdirAll(sc.LogDir, 0755); err != nil {
			return
		}
	}
	sc.handlers = map[string]*Node{}
	if sc.OnExit != nil {
		sc.handlers[constants.OnExit] = &Node{Step: sc.OnExit}
	}
	if sc.OnSuccess != nil {
		sc.handlers[constants.OnSuccess] = &Node{Step: sc.OnSuccess}
	}
	if sc.OnFailure != nil {
		sc.handlers[constants.OnFailure] = &Node{Step: sc.OnFailure}
	}
	if sc.OnCancel != nil {
		sc.handlers[constants.OnCancel] = &Node{Step: sc.OnCancel}
	}
	return
}

func (sc *Scheduler) HanderNode(name string) *Node {
	if v, ok := sc.handlers[name]; ok {
		return v
	}
	return nil
}

func handleError(node *Node) {
	status := node.ReadStatus()
	if status != NodeStatus_Cancel && status != NodeStatus_Success {
		if node.RetryPolicy != nil && node.RetryPolicy.Limit > node.ReadRetryCount() {
			log.Printf("%s failed but scheduled for retry", node.Name)
			node.incRetryCount()
			node.updateStatus(NodeStatus_None)
		} else {
			node.updateStatus(NodeStatus_Error)
		}
	}
}

func (sc *Scheduler) IsCanceled() bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	ret := sc.canceled == 1
	return ret
}

func (sc *Scheduler) setCanceled() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.canceled = 1
}

func (sc *Scheduler) isRunning(g *ExecutionGraph) bool {
	for _, node := range g.Nodes() {
		switch node.ReadStatus() {
		case NodeStatus_Running:
			return true
		}
	}
	return false
}

func (sc *Scheduler) runningCount(g *ExecutionGraph) (count int) {
	count = 0
	for _, node := range g.Nodes() {
		switch node.ReadStatus() {
		case NodeStatus_Running:
			count++
		}
	}
	return count
}

func (sc *Scheduler) isFinished(g *ExecutionGraph) bool {
	for _, node := range g.Nodes() {
		switch node.ReadStatus() {
		case NodeStatus_Running, NodeStatus_None:
			return false
		}
	}
	return true
}

func (sc *Scheduler) checkStatus(g *ExecutionGraph, in []NodeStatus) bool {
	for _, node := range g.Nodes() {
		s := node.ReadStatus()
		var f = false
		for i := range in {
			f = s == in[i]
			if f {
				break
			}
		}
		if !f {
			return false
		}
	}
	return true
}

func (sc *Scheduler) Signal(g *ExecutionGraph, sig os.Signal, done chan bool) {
	if !sc.IsCanceled() {
		sc.setCanceled()
	}
	for _, node := range g.Nodes() {
		if node.RepeatPolicy.Repeat {
			// for a repetitive task, we'll wait for the job to finish
			// until time reaches max wait time
		} else {
			node.signal(sig)
		}
	}
	if done != nil {
		defer func() {
			done <- true
		}()
		for sc.isRunning(g) {
			time.Sleep(sc.pause)
		}
	}
}

func (sc *Scheduler) Cancel(g *ExecutionGraph) {
	sc.setCanceled()
	for _, node := range g.Nodes() {
		node.cancel()
	}
}

func (sc *Scheduler) Status(g *ExecutionGraph) SchedulerStatus {
	if sc.IsCanceled() && !sc.checkStatus(g, []NodeStatus{
		NodeStatus_Success, NodeStatus_Skipped,
	}) {
		return SchedulerStatus_Cancel
	}
	if g.StartedAt.IsZero() {
		return SchedulerStatus_None
	}
	if g.FinishedAt.IsZero() {
		return SchedulerStatus_Running
	}
	if sc.lastError != nil {
		return SchedulerStatus_Error
	}
	return SchedulerStatus_Success
}

func isReady(g *ExecutionGraph, node *Node) (ready bool) {
	ready = true
	for _, dep := range g.To(node.id) {
		n := g.Node(dep)
		switch n.ReadStatus() {
		case NodeStatus_Success:
			continue
		case NodeStatus_Error:
			if !n.ContinueOn.Failure {
				ready = false
				node.updateStatus(NodeStatus_Cancel)
				node.Error = fmt.Errorf("upstream failed")
			}
		case NodeStatus_Skipped:
			if !n.ContinueOn.Skipped {
				ready = false
				node.updateStatus(NodeStatus_Skipped)
				node.Error = fmt.Errorf("upstream skipped")
			}
		case NodeStatus_Cancel:
			ready = false
			node.updateStatus(NodeStatus_Cancel)
		default:
			ready = false
		}
	}
	return ready
}
