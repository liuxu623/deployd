package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/laincloud/deployd/cluster"
	"github.com/laincloud/deployd/storage"
	"github.com/mijia/sweb/log"
)

type PodGroupWithSpec struct {
	Spec      PodGroupSpec
	PrevState []PodPrevState
	PodGroup
}

type podGroupController struct {
	Publisher

	engine *OrcEngine

	opState PGOpState

	sync.RWMutex
	spec      PodGroupSpec
	prevState []PodPrevState
	group     PodGroup

	evSnapshot map[string]RuntimeEaglePod // id => RuntimeEaglePod
	podCtrls   []*podController
	opsChan    chan pgOperation

	refreshable bool

	lastPodSpecKey string
	storedKey      string
	storedKeyDir   string
}

func (pgCtrl *podGroupController) String() string {
	return fmt.Sprintf("PodGroupCtrl %s", pgCtrl.spec)
}

func (pgCtrl *podGroupController) CanOperate(pgops PGOpState) PGOpState {
	pgCtrl.Lock()
	defer pgCtrl.Unlock()
	if pgCtrl.opState == PGOpStateIdle {
		pgCtrl.opState = pgops
		return PGOpStateIdle
	}
	return pgCtrl.opState
}

func (pgCtrl *podGroupController) DisableRefresh() {
	pgCtrl.Lock()
	defer pgCtrl.Unlock()
	pgCtrl.refreshable = false
}

func (pgCtrl *podGroupController) EnableRefresh() {
	pgCtrl.Lock()
	defer pgCtrl.Unlock()
	pgCtrl.refreshable = true
}

// called by signle goroutine
func (pgCtrl *podGroupController) OperateOver() {
	pgCtrl.Lock()
	defer pgCtrl.Unlock()
	pgCtrl.opState = PGOpStateIdle
}

func (pgCtrl *podGroupController) Inspect() PodGroupWithSpec {
	pgCtrl.Lock()
	defer pgCtrl.Unlock()
	return PodGroupWithSpec{pgCtrl.spec, pgCtrl.prevState, pgCtrl.group}
}

func (pgCtrl *podGroupController) IsHealthy() bool {
	pgCtrl.RLock()
	defer pgCtrl.RUnlock()
	for _, pc := range pgCtrl.podCtrls {
		if pc.pod.PodIp() == "" {
			if pc.pod.State == RunStateSuccess {
				ntfController.Send(NewNotifySpec(pc.spec.Namespace, pc.spec.Name, pc.pod.InstanceNo, time.Now(), NotifyPodIPLost))
			}
			return false
		}
	}
	return true
}

func (pgCtrl *podGroupController) IsRemoved() bool {
	pgCtrl.RLock()
	defer pgCtrl.RUnlock()
	return pgCtrl.group.State == RunStateRemoved
}

func (pgCtrl *podGroupController) IsPending() bool {
	pgCtrl.RLock()
	defer pgCtrl.RUnlock()
	return pgCtrl.group.State == RunStatePending
}

func (pgCtrl *podGroupController) Deploy() {
	pgCtrl.RLock()
	spec := pgCtrl.spec.Clone()
	pgCtrl.RUnlock()

	pgCtrl.group.LastError = ""
	if ok := pgCtrl.checkPodPorts(); !ok {
		return
	}

	pgCtrl.opsChan <- pgOperLogOperation{"Start to deploy"}
	pgCtrl.opsChan <- pgOperSaveStore{true}
	pgCtrl.opsChan <- pgOperSnapshotEagleView{spec.Name}
	for i := 0; i < spec.NumInstances; i += 1 {
		pgCtrl.opsChan <- pgOperDeployInstance{i + 1, spec.Version}
	}
	pgCtrl.opsChan <- pgOperSnapshotGroup{true}
	pgCtrl.opsChan <- pgOperSnapshotPrevState{}
	pgCtrl.opsChan <- pgOperSaveStore{true}
	pgCtrl.opsChan <- pgOperLogOperation{"deploy finished"}
	pgCtrl.opsChan <- pgOperOver{}
}

func (pgCtrl *podGroupController) RescheduleInstance(numInstances int, restartPolicy ...RestartPolicy) {
	pgCtrl.RLock()
	spec := pgCtrl.spec.Clone()
	pgCtrl.RUnlock()

	isDirty := false
	curNumInstances := spec.NumInstances
	if numInstances >= 0 && curNumInstances != numInstances {
		spec.NumInstances = numInstances
		isDirty = true
	}
	if len(restartPolicy) > 0 && pgCtrl.spec.RestartPolicy != restartPolicy[0] {
		spec.RestartPolicy = restartPolicy[0]
		isDirty = true
	}
	if !isDirty {
		return
	}

	pgCtrl.Lock()
	pgCtrl.spec = spec
	pgCtrl.Unlock()
	pgCtrl.opsChan <- pgOperLogOperation{fmt.Sprintf("Start to reschedule instance from %d to %d", curNumInstances, numInstances)}
	pgCtrl.opsChan <- pgOperSaveStore{true}
	delta := numInstances - curNumInstances
	if delta != 0 {
		pgCtrl.opsChan <- pgOperSnapshotEagleView{spec.Name}
		if delta > 0 {
			for i := 0; i < delta; i += 1 {
				instanceNo := i + 1 + curNumInstances
				pgCtrl.opsChan <- pgOperPushPodCtrl{spec.Pod}
				pgCtrl.opsChan <- pgOperDeployInstance{instanceNo, spec.Version}
			}
		} else {
			delta *= -1
			for i := 0; i < delta; i += 1 {
				pgCtrl.opsChan <- pgOperRemoveInstance{curNumInstances - i, spec.Pod}
				pgCtrl.opsChan <- pgOperPopPodCtrl{}
			}
		}
		pgCtrl.opsChan <- pgOperSnapshotGroup{true}
		pgCtrl.opsChan <- pgOperSnapshotPrevState{}
		pgCtrl.opsChan <- pgOperSaveStore{true}
	}
	pgCtrl.opsChan <- pgOperLogOperation{"Reschedule instance number finished"}
	pgCtrl.opsChan <- pgOperOver{}
}

func (pgCtrl *podGroupController) RescheduleSpec(podSpec PodSpec) {
	pgCtrl.RLock()
	spec := pgCtrl.spec.Clone()
	pgCtrl.RUnlock()
	// pgCtrl.group.Pods[0].NodeName()
	if spec.Pod.Equals(podSpec) {
		return
	}
	pgCtrl.emptyError()
	if ok := pgCtrl.updatePodPorts(podSpec); !ok {
		return
	}
	// store oldPodSpec for rollback(with ttl 10min)
	pgCtrl.opsChan <- pgOperCacheLastSpec{spec: spec}

	oldPodSpec := spec.Pod.Clone()
	spec.Pod = spec.Pod.Merge(podSpec)
	spec.Version += 1
	spec.UpdatedAt = time.Now()
	pgCtrl.Lock()
	pgCtrl.spec = spec
	pgCtrl.Unlock()
	pgCtrl.opsChan <- pgOperLogOperation{"Start to reschedule spec"}
	pgCtrl.opsChan <- pgOperSaveStore{true}
	pgCtrl.opsChan <- pgOperSnapshotEagleView{spec.Name}
	for i := 0; i < spec.NumInstances; i += 1 {
		pgCtrl.opsChan <- pgOperUpgradeInstance{i + 1, spec.Version, oldPodSpec, spec.Pod}
		pgCtrl.opsChan <- pgOperSnapshotGroup{true}
		pgCtrl.opsChan <- pgOperSaveStore{true}
	}
	pgCtrl.opsChan <- pgOperSnapshotGroup{true}
	pgCtrl.opsChan <- pgOperSnapshotPrevState{}
	pgCtrl.opsChan <- pgOperSaveStore{true}
	pgCtrl.opsChan <- pgOperLogOperation{"Reschedule spec finished"}
	pgCtrl.opsChan <- pgOperOver{}
}

func (pgCtrl *podGroupController) RescheduleDrift(fromNode, toNode string, instanceNo int, force bool) {
	pgCtrl.RLock()
	spec := pgCtrl.spec.Clone()
	pgCtrl.RUnlock()
	if spec.NumInstances == 0 {
		return
	}
	pgCtrl.opsChan <- pgOperLogOperation{fmt.Sprintf("Start to reschedule drift from %s", fromNode)}
	if instanceNo == -1 {
		for i := 0; i < spec.NumInstances; i += 1 {
			pgCtrl.opsChan <- pgOperDriftInstance{i + 1, fromNode, toNode, force}
		}
	} else {
		pgCtrl.opsChan <- pgOperDriftInstance{instanceNo, fromNode, toNode, force}
	}
	pgCtrl.opsChan <- pgOperSnapshotGroup{false}
	pgCtrl.opsChan <- pgOperSnapshotPrevState{}
	pgCtrl.opsChan <- pgOperSaveStore{false}
	pgCtrl.opsChan <- pgOperLogOperation{"Reschedule drift finished"}
	pgCtrl.opsChan <- pgOperOver{}
}

func (pgCtrl *podGroupController) Remove() {
	pgCtrl.RLock()
	spec := pgCtrl.spec.Clone()
	pgCtrl.RUnlock()
	pgCtrl.cancelPodPorts()
	pgCtrl.opsChan <- pgOperLogOperation{"Start to remove"}
	pgCtrl.opsChan <- pgOperRemoveStore{}
	for i := 0; i < spec.NumInstances; i += 1 {
		pgCtrl.opsChan <- pgOperRemoveInstance{i + 1, spec.Pod}
	}
	pgCtrl.opsChan <- pgOperLogOperation{"Remove finished"}
	pgCtrl.opsChan <- pgOperSnapshotEagleView{spec.Name}
	pgCtrl.opsChan <- pgOperPurge{}
	pgCtrl.opsChan <- pgOperOver{}
}

func (pgCtrl *podGroupController) Refresh(force bool) {
	if pgCtrl.IsRemoved() || pgCtrl.IsPending() {
		return
	}
	pgCtrl.RLock()
	spec := pgCtrl.spec.Clone()
	pgCtrl.RUnlock()
	pgCtrl.opsChan <- pgOperLogOperation{"Start to refresh PodGroup"}
	pgCtrl.opsChan <- pgOperSnapshotEagleView{spec.Name}
	for i := 0; i < spec.NumInstances; i += 1 {
		pgCtrl.opsChan <- pgOperRefreshInstance{i + 1, spec}
	}
	pgCtrl.opsChan <- pgOperVerifyInstanceCount{spec}
	pgCtrl.opsChan <- pgOperSnapshotGroup{force}
	pgCtrl.opsChan <- pgOperSnapshotPrevState{}
	pgCtrl.opsChan <- pgOperSaveStore{false}
	pgCtrl.opsChan <- pgOperLogOperation{"PodGroup refreshing finished"}
}

func (pgCtrl *podGroupController) ChangeState(op string, instance int) {
	pgCtrl.RLock()
	spec := pgCtrl.spec.Clone()
	pgCtrl.RUnlock()
	if instance == 0 {
		for i := 0; i < spec.NumInstances; i += 1 {
			pgCtrl.opsChan <- pgOperChangeState{op, i + 1}
		}
	} else if instance > 0 && instance <= spec.NumInstances {
		pgCtrl.opsChan <- pgOperChangeState{op, instance}
	}
	pgCtrl.opsChan <- pgOperSnapshotGroup{true}
	pgCtrl.opsChan <- pgOperSaveStore{true}
	pgCtrl.opsChan <- pgOperOver{}
}

func (pgCtrl *podGroupController) Activate(c cluster.Cluster, store storage.Store, eagle *RuntimeEagleView, stop chan struct{}) {
	go func() {
		for {
			select {
			case op := <-pgCtrl.opsChan:
				toShutdown := op.Do(pgCtrl, c, store, eagle)
				if toShutdown {
					return
				}
			case <-stop:
				if len(pgCtrl.opsChan) == 0 {
					return
				}
			}
		}
	}()
}

/*
 * clean all ops in chan synchronously
 *
 */
func (pgCtrl *podGroupController) FlushAllOps() {
	for {
		if len(pgCtrl.opsChan) == 0 {
			return
		}
		select {
		case <-pgCtrl.opsChan:
		default:
			return
		}
	}
}

func (pgCtrl *podGroupController) LastSpec() *PodGroupSpec {
	log.Infof("Fetch LastPodSpec !")
	var lastSpec PodGroupSpec
	if err := pgCtrl.engine.store.Get(pgCtrl.lastPodSpecKey, &lastSpec); err != nil {
		log.Infof("Fetch LastPodSpec with err:%v", err)
		return nil
	}
	log.Infof("Fetch LastPodSpec :%v", lastSpec)
	return &lastSpec
}

func (pgCtrl *podGroupController) checkUpgradeResult() {

}

/*
 * To clean corrupted containers which do not used by cluster app any more
 * Should be called just after refrehsed podgroups or clean will works terrible
 */
func (pgCtrl *podGroupController) cleanCorruptedContainers() {
	runtimePods := pgCtrl.evSnapshot
	pods := make(map[int][]*container)
	// parse slice runtimePods to map of [instance] => slice containers
	for _, rtPod := range runtimePods {
		instance := rtPod.InstanceNo
		version := rtPod.Version
		driftCount := rtPod.DriftCount
		if pods[instance] == nil {
			pods[instance] = make([]*container, 0)
		}
		pods[instance] = append(pods[instance],
			&container{version: version,
				instance:   instance,
				driftCount: driftCount,
				id:         rtPod.Container.Id,
			})
	}
	corrupted := false
	uselessContainers := make([]*container, 0)
	for instance, containers := range pods {
		if instance > pgCtrl.spec.NumInstances {
			corrupted = true
			for _, container := range containers {
				uselessContainers = append(uselessContainers, container)
			}
		} else if len(containers) > 1 {
			By(ByVersionAndDriftCounter).Sort(containers)
			corrupted = true
			for _, container := range containers[1:] {
				uselessContainers = append(uselessContainers, container)
			}
		}
	}
	if corrupted {
		ids := make([]string, len(uselessContainers))
		for i, container := range uselessContainers {
			log.Infof("need remove container:%v", container)
			// remove container in cluster
			delete(pgCtrl.evSnapshot, container.id)
			ids[i] = container.id
		}
		go removeContainers(pgCtrl.engine.cluster, ids)
	}
}

func removeContainers(c cluster.Cluster, ids []string) {
	for _, cId := range ids {
		log.Warnf("find some corrupted container alive, try to remove it")
		if err := c.RemoveContainer(cId, true, false); err != nil {
			log.Warnf("still cannot remove the container ")
		}
	}
}

func (pgCtrl *podGroupController) emitChangeEvent(changeType string, spec PodSpec, pod Pod, nodeName string) {
	if changeType == "" || nodeName == "" {
		return
	}
	var events []interface{}
	namespace := spec.Namespace
	for _, dep := range spec.Dependencies {
		if dep.Policy == DependencyNodeLevel {
			namespace = fmt.Sprintf("%s", nodeName)
		}
		events = append(events, DependencyEvent{
			Type:      changeType,
			Name:      dep.PodName,
			NodeName:  nodeName,
			Namespace: namespace,
		})
	}
	log.Debugf("%s emit change event: %s, %q, #evts=%d", pgCtrl, changeType, nodeName, len(events))
	for _, evt := range events {
		pgCtrl.EmitEvent(evt)
	}
}

func (pgCtrl *podGroupController) cancelPodPorts() {
	spec := pgCtrl.spec
	var sps StreamPorts
	if err := json.Unmarshal([]byte(spec.Pod.Annotation), &sps); err != nil {
		log.Errorf("annotation unmarshal error:%v\n", err)
		return
	}
	stProc := make([]*StreamProc, 0, len(sps.Ports))
	for _, sp := range sps.Ports {
		stProc = append(stProc, &StreamProc{
			StreamPort: sp,
			NameSpace:  pgCtrl.spec.Namespace,
			ProcName:   pgCtrl.spec.Name,
		})
	}
	CancelPorts(stProc...)
}

func (pgCtrl *podGroupController) checkPodPorts() bool {
	spec := pgCtrl.spec
	var sps StreamPorts
	if err := json.Unmarshal([]byte(spec.Pod.Annotation), &sps); err != nil {
		log.Errorf("annotation unmarshal, %v, error:%v\n", spec.Pod.Annotation, err)
		return false
	}
	stProc := make([]*StreamProc, 0, len(sps.Ports))
	for _, sp := range sps.Ports {
		stProc = append(stProc, &StreamProc{
			StreamPort: sp,
			NameSpace:  pgCtrl.spec.Namespace,
			ProcName:   pgCtrl.spec.Name,
		})
	}
	succ, existsPorts := RegisterPorts(stProc...)
	if succ {
		return true
	} else {
		pgCtrl.group.State = RunStateFail
		pgCtrl.group.LastError = fmt.Sprintf("Cannot start podgroup %v, some ports like %v were alerady in used!", pgCtrl.spec.Name, existsPorts)
		return false
	}
	return true
}

func (pgCtrl *podGroupController) updatePodPorts(podSpec PodSpec) bool {
	spec := pgCtrl.spec
	if spec.Pod.Equals(podSpec) {
		return true
	}
	var oldsps, sps StreamPorts
	if err := json.Unmarshal([]byte(spec.Pod.Annotation), &oldsps); err != nil {
		log.Errorf("annotation unmarshal error:%v\n", err)
		return false
	}
	if err := json.Unmarshal([]byte(podSpec.Annotation), &sps); err != nil {
		log.Errorf("annotation unmarshal error:%v\n", err)
		return false
	}
	if !oldsps.Equals(sps) {
		//register fresh ports && cancel dated ports
		freshArr := make([]*StreamProc, 0)
		datedArr := make([]*StreamProc, 0)
		updateArr := make([]*StreamProc, 0)
		var exists bool
		for _, fresh := range sps.Ports {
			exists = false
			for _, dated := range oldsps.Ports {
				if dated.Equals(fresh) {
					exists = true
					break
				} else if dated.SrcPort == fresh.SrcPort {
					exists = true
					updateArr = append(freshArr, &StreamProc{
						StreamPort: fresh,
						NameSpace:  pgCtrl.spec.Namespace,
						ProcName:   pgCtrl.spec.Name,
					})
					break
				}
			}
			if !exists {
				freshArr = append(freshArr, &StreamProc{
					StreamPort: fresh,
					NameSpace:  pgCtrl.spec.Namespace,
					ProcName:   pgCtrl.spec.Name,
				})
			}
		}

		for _, dated := range oldsps.Ports {
			exists = false
			for _, fresh := range sps.Ports {
				if dated.SrcPort == fresh.SrcPort {
					exists = true
					break
				}
			}
			if !exists {
				datedArr = append(datedArr, &StreamProc{
					StreamPort: dated,
					NameSpace:  pgCtrl.spec.Namespace,
					ProcName:   pgCtrl.spec.Name,
				})
			}
		}

		succ, existsPorts := RegisterPorts(freshArr...)
		if succ {
			UpdatePorts(updateArr...)
			CancelPorts(datedArr...)
			return true
		} else {
			pgCtrl.group.State = RunStateFail
			pgCtrl.group.LastError = fmt.Sprintf("Cannot start podgroup %v, some ports like %v were alerady in used!", pgCtrl.spec.Name, existsPorts)
			return false
		}
	}
	return true
}

func (pgCtrl *podGroupController) waitLastPodHealth(i int) bool {
	retries := 5
	retryTimes := 0
	if i > 0 {
		sleepTime := DefaultSetUpTime
		podSpec := pgCtrl.spec.Pod
		if podSpec.GetSetupTime() > DefaultSetUpTime {
			sleepTime = podSpec.GetSetupTime()
		}
		// wait some seconds for new instance's initialization completed, before we update next one
		if pgCtrl.podCtrls[i].pod.Healthst == HealthStateNone {
			time.Sleep(time.Second * time.Duration(podSpec.GetSetupTime()))
		} else {
			for {
				if retryTimes == retries {
					break
				}
				retryTimes++
				// wait until to healthy state
				if pgCtrl.podCtrls[i-1].pod.Healthst == HealthStateHealthy {
					break
				}
				time.Sleep(time.Second * time.Duration(sleepTime))
			}
		}
	}
	// reset health state to starting
	if pgCtrl.podCtrls[i].pod.Healthst != HealthStateNone {
		pgCtrl.podCtrls[i].pod.Healthst = HealthStateStarting
	}
	if retryTimes == retries && pgCtrl.podCtrls[i-1].pod.Healthst != HealthStateHealthy {
		return false
	}
	return true
}

func (pgCtrl *podGroupController) emptyError() {
	pgCtrl.Lock()
	defer pgCtrl.Unlock()
	pgCtrl.group.LastError = ""
}

func newPodGroupController(spec PodGroupSpec, states []PodPrevState, pg PodGroup, engine *OrcEngine) *podGroupController {
	podCtrls := make([]*podController, spec.NumInstances)
	for i := range podCtrls {
		var pod Pod
		pod.InstanceNo = i + 1
		pod.State = RunStatePending
		podSpec := spec.Pod.Clone()
		if states != nil && i < len(states) {
			podSpec.PrevState = states[i].Clone() // set the pod's prev state
		} else {
			podSpec.PrevState = NewPodPrevState(1) // set empty prev state
		}
		podCtrls[i] = &podController{
			spec: podSpec,
			pod:  pod,
		}
	}
	// we may have some running pods loading from the storage
	for _, pod := range pg.Pods {
		if pod.InstanceNo < 1 || pod.InstanceNo > spec.NumInstances {
			log.Warnf("We have some pod have InstanceNo out of bounds, %d, bounds=[0, %d]", pod.InstanceNo, spec.NumInstances)
			continue
		}
		podCtrls[pod.InstanceNo-1].pod = pod
	}

	pgCtrl := &podGroupController{
		engine:   engine,
		opState:  PGOpStateIdle,
		spec:     spec,
		group:    pg,
		podCtrls: podCtrls,
		opsChan:  make(chan pgOperation, 500),

		refreshable: true,

		lastPodSpecKey: strings.Join([]string{kLainDeploydRootKey, kLainLastPodSpecKey, spec.Namespace, spec.Name}, "/"),
		storedKey:      strings.Join([]string{kLainDeploydRootKey, kLainPodGroupKey, spec.Namespace, spec.Name}, "/"),
		storedKeyDir:   strings.Join([]string{kLainDeploydRootKey, kLainPodGroupKey, spec.Namespace}, "/"),
	}
	pgCtrl.Publisher = NewPublisher(true)
	return pgCtrl
}
