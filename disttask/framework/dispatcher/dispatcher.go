// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dispatcher

import (
	"context"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/disttask/framework/proto"
	"github.com/pingcap/tidb/disttask/framework/storage"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/resourcemanager/pool/spool"
	"github.com/pingcap/tidb/resourcemanager/util"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	tidbutil "github.com/pingcap/tidb/util"
	disttaskutil "github.com/pingcap/tidb/util/disttask"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/syncutil"
	"go.uber.org/zap"
)

const (
	// DefaultSubtaskConcurrency is the default concurrency for handling subtask.
	DefaultSubtaskConcurrency = 16
	// MaxSubtaskConcurrency is the maximum concurrency for handling subtask.
	MaxSubtaskConcurrency = 256
)

var (
	// DefaultDispatchConcurrency is the default concurrency for handling global task.
	DefaultDispatchConcurrency = 4
	checkTaskFinishedInterval  = 500 * time.Millisecond
	checkTaskRunningInterval   = 300 * time.Millisecond
	nonRetrySQLTime            = 1
	retrySQLTimes              = variable.DefTiDBDDLErrorCountLimit
	retrySQLInterval           = 500 * time.Millisecond
)

// Dispatch defines the interface for operations inside a dispatcher.
type Dispatch interface {
	// Start enables dispatching and monitoring mechanisms.
	Start()
	// GetAllSchedulerIDs gets handles the task's all available instances.
	GetAllSchedulerIDs(ctx context.Context, gTaskID int64) ([]string, error)
	// Stop stops the dispatcher.
	Stop()
}

// TaskHandle provides the interface for operations needed by task flow handles.
type TaskHandle interface {
	// GetAllSchedulerIDs gets handles the task's all scheduler instances.
	GetAllSchedulerIDs(ctx context.Context, gTaskID int64) ([]string, error)
	// GetPreviousSubtaskMetas gets previous subtask metas.
	GetPreviousSubtaskMetas(gTaskID int64, step int64) ([][]byte, error)
	// ExecInNewSession executes the function with a new session.
	ExecInNewSession(fn func(se sessionctx.Context) error) error
}

func (d *dispatcher) getRunningGTaskCnt() int {
	d.runningGTasks.RLock()
	defer d.runningGTasks.RUnlock()
	return len(d.runningGTasks.taskIDs)
}

func (d *dispatcher) setRunningGTask(gTask *proto.Task) {
	d.runningGTasks.Lock()
	d.runningGTasks.taskIDs[gTask.ID] = struct{}{}
	d.runningGTasks.Unlock()
	d.detectPendingGTaskCh <- gTask
}

func (d *dispatcher) isRunningGTask(globalTaskID int64) bool {
	d.runningGTasks.Lock()
	defer d.runningGTasks.Unlock()
	_, ok := d.runningGTasks.taskIDs[globalTaskID]
	return ok
}

func (d *dispatcher) delRunningGTask(globalTaskID int64) {
	d.runningGTasks.Lock()
	defer d.runningGTasks.Unlock()
	delete(d.runningGTasks.taskIDs, globalTaskID)
}

type dispatcher struct {
	ctx     context.Context
	cancel  context.CancelFunc
	taskMgr *storage.TaskManager
	wg      tidbutil.WaitGroupWrapper
	gPool   *spool.Pool

	runningGTasks struct {
		syncutil.RWMutex
		taskIDs map[int64]struct{}
	}
	detectPendingGTaskCh chan *proto.Task
}

// NewDispatcher creates a dispatcher struct.
func NewDispatcher(ctx context.Context, taskTable *storage.TaskManager) (Dispatch, error) {
	dispatcher := &dispatcher{
		taskMgr:              taskTable,
		detectPendingGTaskCh: make(chan *proto.Task, DefaultDispatchConcurrency),
	}
	pool, err := spool.NewPool("dispatch_pool", int32(DefaultDispatchConcurrency), util.DistTask, spool.WithBlocking(true))
	if err != nil {
		return nil, err
	}
	dispatcher.gPool = pool
	dispatcher.ctx, dispatcher.cancel = context.WithCancel(ctx)
	dispatcher.runningGTasks.taskIDs = make(map[int64]struct{})

	return dispatcher, nil
}

// Start implements Dispatch.Start interface.
func (d *dispatcher) Start() {
	d.wg.Run(d.DispatchTaskLoop)
	d.wg.Run(d.DetectTaskLoop)
}

// Stop implements Dispatch.Stop interface.
func (d *dispatcher) Stop() {
	d.cancel()
	d.gPool.ReleaseAndWait()
	d.wg.Wait()
}

// DispatchTaskLoop dispatches the global tasks.
func (d *dispatcher) DispatchTaskLoop() {
	logutil.BgLogger().Info("dispatch task loop start")
	ticker := time.NewTicker(checkTaskRunningInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			logutil.BgLogger().Info("dispatch task loop exits", zap.Error(d.ctx.Err()), zap.Int64("interval", int64(checkTaskRunningInterval)/1000000))
			return
		case <-ticker.C:
			cnt := d.getRunningGTaskCnt()
			if d.checkConcurrencyOverflow(cnt) {
				break
			}

			// TODO: Consider getting these tasks, in addition to the task being worked on..
			gTasks, err := d.taskMgr.GetGlobalTasksInStates(proto.TaskStatePending, proto.TaskStateRunning, proto.TaskStateReverting, proto.TaskStateCancelling)
			if err != nil {
				logutil.BgLogger().Warn("get unfinished(pending, running or reverting) tasks failed", zap.Error(err))
				break
			}

			// There are currently no global tasks to work on.
			if len(gTasks) == 0 {
				break
			}
			for _, gTask := range gTasks {
				// This global task is running, so no need to reprocess it.
				if d.isRunningGTask(gTask.ID) {
					continue
				}
				// the task is not in runningGTasks set when:
				// owner changed or task is cancelled when status is pending
				if gTask.State == proto.TaskStateRunning || gTask.State == proto.TaskStateReverting || gTask.State == proto.TaskStateCancelling {
					d.setRunningGTask(gTask)
					cnt++
					continue
				}

				if d.checkConcurrencyOverflow(cnt) {
					break
				}

				err = d.processNormalFlow(gTask)
				logutil.BgLogger().Info("dispatch task loop", zap.Int64("task ID", gTask.ID),
					zap.String("state", gTask.State), zap.Uint64("concurrency", gTask.Concurrency), zap.Error(err))
				if err != nil || gTask.IsFinished() {
					continue
				}
				d.setRunningGTask(gTask)
				cnt++
			}
		}
	}
}

func (d *dispatcher) probeTask(gTask *proto.Task) (isFinished bool, subTaskErr [][]byte) {
	// TODO: Consider putting the following operations into a transaction.
	// TODO: Consider collect some information about the tasks.
	if gTask.State != proto.TaskStateReverting {
		// check if global task cancelling
		cancelling, err := d.taskMgr.IsGlobalTaskCancelling(gTask.ID)
		if err != nil {
			logutil.BgLogger().Warn("check task cancelling failed", zap.Int64("task ID", gTask.ID), zap.Error(err))
			return false, nil
		}

		if cancelling {
			return false, [][]byte{[]byte("cancel")}
		}
		// check subtasks failed
		cnt, err := d.taskMgr.GetSubtaskInStatesCnt(gTask.ID, proto.TaskStateFailed)
		if err != nil {
			logutil.BgLogger().Warn("check task failed", zap.Int64("task ID", gTask.ID), zap.Error(err))
			return false, nil
		}
		if cnt > 0 {
			subTaskErr, err = d.taskMgr.CollectSubTaskError(gTask.ID)
			if err != nil {
				logutil.BgLogger().Warn("collate subtask error failed", zap.Int64("task ID", gTask.ID), zap.Error(err))
				return false, nil
			}
			return false, subTaskErr
		}
		// check subtasks pending or running
		cnt, err = d.taskMgr.GetSubtaskInStatesCnt(gTask.ID, proto.TaskStatePending, proto.TaskStateRunning)
		if err != nil {
			logutil.BgLogger().Warn("check task failed", zap.Int64("task ID", gTask.ID), zap.Error(err))
			return false, nil
		}
		if cnt > 0 {
			logutil.BgLogger().Info("check task, subtasks aren't finished", zap.Int64("task ID", gTask.ID), zap.Int64("cnt", cnt))
			return false, nil
		}
		return true, nil
	}

	// if gTask.State == TaskStateReverting, if will not convert to TaskStateCancelling again
	cnt, err := d.taskMgr.GetSubtaskInStatesCnt(gTask.ID, proto.TaskStateRevertPending, proto.TaskStateReverting)
	if err != nil {
		logutil.BgLogger().Warn("check task failed", zap.Int64("task ID", gTask.ID), zap.Error(err))
		return false, nil
	}
	if cnt > 0 {
		return false, nil
	}
	return true, nil
}

// DetectTaskLoop monitors the status of the subtasks and processes them.
func (d *dispatcher) DetectTaskLoop() {
	logutil.BgLogger().Info("detect task loop start")
	for {
		select {
		case <-d.ctx.Done():
			logutil.BgLogger().Info("detect task loop exits", zap.Error(d.ctx.Err()))
			return
		case task := <-d.detectPendingGTaskCh:
			// Using the pool with block, so it wouldn't return an error.
			_ = d.gPool.Run(func() { d.detectTask(task) })
		}
	}
}

func (d *dispatcher) detectTask(gTask *proto.Task) {
	ticker := time.NewTicker(checkTaskFinishedInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.ctx.Done():
			logutil.BgLogger().Info("detect task exits", zap.Int64("task ID", gTask.ID), zap.Error(d.ctx.Err()))
			return
		case <-ticker.C:
			// TODO: Consider actively obtaining information about task completion.
			stepIsFinished, errStr := d.probeTask(gTask)
			// The global task isn't finished and not failed.
			if !stepIsFinished && len(errStr) == 0 {
				GetTaskFlowHandle(gTask.Type).OnTicker(d.ctx, gTask)
				logutil.BgLogger().Debug("detect task, this task keeps current state",
					zap.Int64("taskID", gTask.ID), zap.String("state", gTask.State))
				break
			}

			err := d.processFlow(gTask, errStr)
			if err == nil && gTask.IsFinished() {
				logutil.BgLogger().Info("detect task, task is finished",
					zap.Int64("taskID", gTask.ID), zap.String("state", gTask.State))
				d.delRunningGTask(gTask.ID)
				return
			}
			if !d.isRunningGTask(gTask.ID) {
				logutil.BgLogger().Info("detect task, this task can't run",
					zap.Int64("taskID", gTask.ID), zap.String("state", gTask.State))
			}
		}
	}
}

func (d *dispatcher) processFlow(gTask *proto.Task, errStr [][]byte) error {
	if len(errStr) > 0 {
		// Found an error when task is running.
		logutil.BgLogger().Info("process flow, handle an error", zap.Int64("taskID", gTask.ID), zap.Any("err msg", errStr))
		return d.processErrFlow(gTask, errStr)
	}
	// previous step is finished
	if gTask.State == proto.TaskStateReverting {
		// Finish the rollback step.
		logutil.BgLogger().Info("process flow, update the task to reverted", zap.Int64("taskID", gTask.ID))
		return d.updateTask(gTask, proto.TaskStateReverted, nil, retrySQLTimes)
	}
	// Finish the normal step.
	logutil.BgLogger().Info("process flow, process normal", zap.Int64("taskID", gTask.ID))
	return d.processNormalFlow(gTask)
}

func (d *dispatcher) updateTask(gTask *proto.Task, gTaskState string, newSubTasks []*proto.Subtask, retryTimes int) (err error) {
	prevState := gTask.State
	gTask.State = gTaskState
	for i := 0; i < retryTimes; i++ {
		err = d.taskMgr.UpdateGlobalTaskAndAddSubTasks(gTask, newSubTasks, gTaskState == proto.TaskStateReverting)
		if err == nil {
			break
		}
		if i%10 == 0 {
			logutil.BgLogger().Warn("updateTask first failed", zap.Int64("taskID", gTask.ID),
				zap.String("previous state", prevState), zap.String("curr state", gTask.State),
				zap.Int("retry times", retryTimes), zap.Error(err))
		}
		time.Sleep(retrySQLInterval)
	}
	if err != nil && retryTimes != nonRetrySQLTime {
		logutil.BgLogger().Warn("updateTask failed and delete running task info", zap.Int64("taskID", gTask.ID),
			zap.String("previous state", prevState), zap.String("curr state", gTask.State), zap.Int("retry times", retryTimes), zap.Error(err))
		d.delRunningGTask(gTask.ID)
	}
	return err
}

func (d *dispatcher) processErrFlow(gTask *proto.Task, receiveErr [][]byte) error {
	// TODO: Maybe it gets GetTaskFlowHandle fails when rolling upgrades.
	// 1. generate the needed global task meta and subTask meta (dist-plan)
	meta, err := GetTaskFlowHandle(gTask.Type).ProcessErrFlow(d.ctx, d, gTask, receiveErr)
	if err != nil {
		logutil.BgLogger().Warn("handle error failed", zap.Error(err))
		return err
	}

	// 2. dispatch revert dist-plan to EligibleInstances
	return d.dispatchSubTask4Revert(gTask, meta)
}

func (d *dispatcher) dispatchSubTask4Revert(gTask *proto.Task, meta []byte) error {
	instanceIDs, err := d.GetAllSchedulerIDs(d.ctx, gTask.ID)
	if err != nil {
		logutil.BgLogger().Warn("get global task's all instances failed", zap.Error(err))
		return err
	}

	if len(instanceIDs) == 0 {
		return d.updateTask(gTask, proto.TaskStateReverted, nil, retrySQLTimes)
	}

	subTasks := make([]*proto.Subtask, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		subTasks = append(subTasks, proto.NewSubtask(gTask.ID, gTask.Type, id, meta))
	}
	return d.updateTask(gTask, proto.TaskStateReverting, subTasks, retrySQLTimes)
}

func (d *dispatcher) processNormalFlow(gTask *proto.Task) error {
	// 1. generate the needed global task meta and subTask meta (dist-plan)
	handle := GetTaskFlowHandle(gTask.Type)
	if handle == nil {
		logutil.BgLogger().Warn("gen gTask flow handle failed, this type handle doesn't register", zap.Int64("ID", gTask.ID), zap.String("type", gTask.Type))
		return d.updateTask(gTask, proto.TaskStateReverted, nil, retrySQLTimes)
	}
	metas, err := handle.ProcessNormalFlow(d.ctx, d, gTask)
	if err != nil {
		logutil.BgLogger().Warn("gen dist-plan failed", zap.Error(err))
		if handle.IsRetryableErr(err) {
			return err
		}
		gTask.Error = []byte(err.Error())
		return d.updateTask(gTask, proto.TaskStateReverted, nil, retrySQLTimes)
	}
	logutil.BgLogger().Info("process normal flow", zap.Int64("task ID", gTask.ID),
		zap.String("state", gTask.State), zap.Uint64("concurrency", gTask.Concurrency), zap.Int("subtasks", len(metas)))

	// 2. dispatch dist-plan to EligibleInstances
	return d.dispatchSubTask(gTask, handle, metas)
}

func (d *dispatcher) dispatchSubTask(gTask *proto.Task, handle TaskFlowHandle, metas [][]byte) error {
	// Adjust the global task's concurrency.
	if gTask.Concurrency == 0 {
		gTask.Concurrency = DefaultSubtaskConcurrency
	}
	if gTask.Concurrency > MaxSubtaskConcurrency {
		gTask.Concurrency = MaxSubtaskConcurrency
	}

	retryTimes := retrySQLTimes
	// Special handling for the new tasks.
	if gTask.State == proto.TaskStatePending {
		// TODO: Consider using TS.
		nowTime := time.Now().UTC()
		gTask.StartTime = nowTime
		gTask.State = proto.TaskStateRunning
		gTask.StateUpdateTime = nowTime
		retryTimes = nonRetrySQLTime
	}

	if len(metas) == 0 {
		gTask.StateUpdateTime = time.Now().UTC()
		// Write the global task meta into the storage.
		err := d.updateTask(gTask, proto.TaskStateSucceed, nil, retryTimes)
		if err != nil {
			logutil.BgLogger().Warn("update global task failed", zap.Error(err))
			return err
		}
		return nil
	}
	// select all available TiDB nodes for this global tasks.
	serverNodes, err1 := handle.GetEligibleInstances(d.ctx, gTask)
	if err1 != nil {
		return err1
	}
	if len(serverNodes) == 0 {
		return errors.New("no available TiDB node")
	}
	subTasks := make([]*proto.Subtask, 0, len(metas))
	for i, meta := range metas {
		// we assign the subtask to the instance in a round-robin way.
		pos := i % len(serverNodes)
		instanceID := disttaskutil.GenerateExecID(serverNodes[pos].IP, serverNodes[pos].Port)
		subTasks = append(subTasks, proto.NewSubtask(gTask.ID, gTask.Type, instanceID, meta))
	}
	return d.updateTask(gTask, gTask.State, subTasks, retrySQLTimes)
}

// GenerateSchedulerNodes generate a eligible TiDB nodes.
func GenerateSchedulerNodes(ctx context.Context) ([]*infosync.ServerInfo, error) {
	serverInfos, err := infosync.GetAllServerInfo(ctx)
	if err != nil {
		return nil, err
	}
	if len(serverInfos) == 0 {
		return nil, errors.New("not found instance")
	}

	serverNodes := make([]*infosync.ServerInfo, 0, len(serverInfos))
	for _, serverInfo := range serverInfos {
		serverNodes = append(serverNodes, serverInfo)
	}
	return serverNodes, nil
}

// GetAllSchedulerIDs gets all the scheduler IDs.
func (d *dispatcher) GetAllSchedulerIDs(ctx context.Context, gTaskID int64) ([]string, error) {
	serverInfos, err := infosync.GetAllServerInfo(ctx)
	if err != nil {
		return nil, err
	}
	if len(serverInfos) == 0 {
		return nil, nil
	}

	schedulerIDs, err := d.taskMgr.GetSchedulerIDsByTaskID(gTaskID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(schedulerIDs))
	for _, id := range schedulerIDs {
		if ok := matchServerInfo(serverInfos, id); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func matchServerInfo(serverInfos map[string]*infosync.ServerInfo, schedulerID string) bool {
	for _, serverInfo := range serverInfos {
		serverID := disttaskutil.GenerateExecID(serverInfo.IP, serverInfo.Port)
		if serverID == schedulerID {
			return true
		}
	}
	return false
}

func (d *dispatcher) GetPreviousSubtaskMetas(gTaskID int64, step int64) ([][]byte, error) {
	previousSubtasks, err := d.taskMgr.GetSucceedSubtasksByStep(gTaskID, step)
	if err != nil {
		logutil.BgLogger().Warn("get previous succeed subtask failed", zap.Int64("ID", gTaskID), zap.Int64("step", step))
		return nil, err
	}
	previousSubtaskMetas := make([][]byte, 0, len(previousSubtasks))
	for _, subtask := range previousSubtasks {
		previousSubtaskMetas = append(previousSubtaskMetas, subtask.Meta)
	}
	return previousSubtaskMetas, nil
}

func (d *dispatcher) ExecInNewSession(fn func(se sessionctx.Context) error) error {
	return d.taskMgr.WithNewSession(fn)
}

func (*dispatcher) checkConcurrencyOverflow(cnt int) bool {
	if cnt >= DefaultDispatchConcurrency {
		logutil.BgLogger().Info("dispatch task loop, running GTask cnt is more than concurrency",
			zap.Int("running cnt", cnt), zap.Int("concurrency", DefaultDispatchConcurrency))
		return true
	}
	return false
}
