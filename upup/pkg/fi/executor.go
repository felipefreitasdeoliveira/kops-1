/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fi

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
)

type executor struct {
	context *Context
}

type taskState struct {
	done         bool
	key          string
	task         Task
	deadline     time.Time
	lastError    error
	dependencies []*taskState
}

// RunTasks executes all the tasks, considering their dependencies
// It will perform some re-execution on error, retrying as long as progress is still being made
func (e *executor) RunTasks(taskMap map[string]Task, maxTaskDuration time.Duration) error {
	dependencies := FindTaskDependencies(taskMap)

	taskStates := make(map[string]*taskState)

	for k, task := range taskMap {
		ts := &taskState{
			key:  k,
			task: task,
		}
		taskStates[k] = ts
	}

	for k, ts := range taskStates {
		for _, dep := range dependencies[k] {
			d := taskStates[dep]
			if d == nil {
				glog.Fatalf("did not find task state for dependency: %q", k)
			}
			ts.dependencies = append(ts.dependencies, d)
		}
	}

	for {
		var canRun []*taskState
		doneCount := 0
		for _, ts := range taskStates {
			if ts.done {
				doneCount++
				continue
			}
			ready := true
			for _, dep := range ts.dependencies {
				if !dep.done {
					ready = false
					break
				}
			}
			if ready {
				if ts.deadline.IsZero() {
					ts.deadline = time.Now().Add(maxTaskDuration)
				} else if time.Now().After(ts.deadline) {
					return fmt.Errorf("deadline exceeded executing task %v. Example error: %v", ts.key, ts.lastError)
				}
				canRun = append(canRun, ts)
			}
		}

		glog.Infof("Tasks: %d done / %d total; %d can run", doneCount, len(taskStates), len(canRun))
		if len(canRun) == 0 {
			break
		}

		progress := false

		var tasks []*taskState
		for _, ts := range canRun {
			tasks = append(tasks, ts)
		}

		taskErrors := e.forkJoin(tasks)
		var errors []error
		for i, err := range taskErrors {
			ts := tasks[i]
			if err != nil {
				glog.Warningf("error running task %q (%v remaining to succeed): %v", ts.key, ts.deadline.Sub(time.Now()), err)
				errors = append(errors, err)
				ts.lastError = err
			} else {
				ts.done = true
				ts.lastError = nil
				progress = true
			}
		}

		if !progress {
			if len(errors) == 0 {
				// Logic error!
				panic("did not make progress executing tasks; but no errors reported")
			}
			glog.Infof("No progress made, sleeping before retrying %d failed task(s)", len(errors))
			time.Sleep(10 * time.Second)
		}
	}

	// Raise error if not all tasks done - this means they depended on each other
	var notDone []string
	for _, ts := range taskStates {
		if !ts.done {
			notDone = append(notDone, ts.key)
		}
	}
	if len(notDone) != 0 {
		return fmt.Errorf("Unable to execute tasks (circular dependency): %s", strings.Join(notDone, ", "))
	}

	return nil
}

type runnable func() error

func (e *executor) forkJoin(tasks []*taskState) []error {
	if len(tasks) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	results := make([]error, len(tasks))
	for i := 0; i < len(tasks); i++ {
		wg.Add(1)
		go func(ts *taskState, index int) {
			results[index] = fmt.Errorf("function panic")
			defer wg.Done()
			glog.V(2).Infof("Executing task %q: %v\n", ts.key, ts.task)
			results[index] = ts.task.Run(e.context)
		}(tasks[i], i)
	}

	wg.Wait()

	return results
}
