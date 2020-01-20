package kuberneteswatcher

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"statusbay/serverutil"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	appsV1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
)

type Resources struct {
	Deployments map[string]*DeploymentData `json:"Deployments"`
	Daemonsets  map[string]*DaemonsetData  `json:"Daemonsets"`
}

//DBSchema is a struct that save as json in given storage
type DBSchema struct {
	Application           string                      `json:"Application"`
	Cluster               string                      `json:"Cluster"`
	Namespace             string                      `json:"Namespace"`
	CreationTimestamp     int64                       `json:"CreationTimestamp"`
	ReportTo              []string                    `json:"ReportTo"`
	DeployBy              string                      `json:"DeployBy"`
	DeploymentDescription DeploymentStatusDescription `json:"DeploymentDescription"`
	Resources             Resources                   `json:"Resources"`
}

// RegistryRow defined row data of deployment
type RegistryRow struct {
	// registory
	id                               uint
	finish                           bool
	status                           DeploymentStatus
	ctx                              context.Context
	cancelFn                         context.CancelFunc
	collectDataAfterDeploymentFinish time.Duration
	DBSchema                         DBSchema
}

// RegistryManager defined multiple rows data
type RegistryManager struct {
	registryData                     map[string]*RegistryRow
	saveInterval                     time.Duration
	saveDeploymentHistoryDuration    time.Duration
	checkFinishDelay                 time.Duration
	collectDataAfterDeploymentFinish time.Duration
	saveLock                         *sync.Mutex
	newAppLock                       *sync.Mutex
	storage                          Storage
	reporter                         *ReporterManager
	lastDeploymentHistory            map[string]time.Time
}

func (dr *RegistryManager) UpdateAppliesVersionHistory(name, namespace string, hash uint64) bool {
	return dr.storage.UpdateAppliesVersionHistory(fmt.Sprintf("%s-%s", namespace, name), hash)
}

func (dr *RegistryManager) DeleteAppliedVersion(name, namespace string) bool {
	return dr.storage.DeleteAppliedVersion(fmt.Sprintf("%s-%s", namespace, name))
}

// NewRegistryManager create new schema registry instance
func NewRegistryManager(saveInterval time.Duration, saveDeploymentHistoryDuration time.Duration, checkFinishDelay time.Duration, collectDataAfterDeploymentFinish time.Duration, storage Storage, reporter *ReporterManager) *RegistryManager {
	return &RegistryManager{
		saveInterval:                     saveInterval,
		saveDeploymentHistoryDuration:    saveDeploymentHistoryDuration,
		checkFinishDelay:                 checkFinishDelay,
		collectDataAfterDeploymentFinish: collectDataAfterDeploymentFinish,
		storage:                          storage,
		reporter:                         reporter,

		registryData:          make(map[string]*RegistryRow),
		lastDeploymentHistory: make(map[string]time.Time),
		saveLock:              &sync.Mutex{},
		newAppLock:            &sync.Mutex{},
	}
}

// Serve will start listening schema registry request
func (dr *RegistryManager) Serve() serverutil.StopFunc {

	ctx, cancelFn := context.WithCancel(context.Background())
	stopped := make(chan bool)
	go func() {
		for {
			select {
			case <-time.After(dr.saveInterval):
				dr.save()
			case <-ctx.Done():
				log.Warn("Registry save schema has been shut down")
				stopped <- true
				return
			}
		}
	}()

	return func() {
		cancelFn()
		<-stopped
	}
}

// LoadRunningApps TODO:: fix me
func (dr *RegistryManager) LoadRunningApplies() []*RegistryRow {

	rows := []*RegistryRow{}
	apps, _ := dr.storage.GetAppliesByStatus(DeploymentStatusRunning)
	log.WithField("count", len(apps)).Info("Loading running job from DB")

	for id, appSchema := range apps {

		encodedID := generateID(appSchema.Application, appSchema.Namespace)
		ctx, cancelFn := context.WithCancel(context.Background())

		row := RegistryRow{
			id:       id,
			ctx:      ctx,
			cancelFn: cancelFn,
			finish:   false,
			status:   DeploymentStatusRunning,
			DBSchema: appSchema,
		}
		go row.isFinish(dr.checkFinishDelay)
		dr.registryData[encodedID] = &row

		rows = append(rows, &row)

	}

	return rows

}

// NewApplication will creates a new deployment row
func (dr *RegistryManager) NewApplication(
	appName string,
	resourceName string,
	namespace string,
	clusterName string,
	annotations map[string]string,
	status DeploymentStatus) *RegistryRow {
	dr.newAppLock.Lock()
	defer dr.newAppLock.Unlock()

	encodedID := generateID(appName, namespace)
	reportTo := GetMetadataByPrefix(annotations, fmt.Sprintf("%s/%s", METAPREFIX, "report-"))
	deployBy := GetMetadata(annotations, fmt.Sprintf("%s/%s", METAPREFIX, "report-deploy-by"))
	deployTime := time.Now().Unix()
	ctx, cancelFn := context.WithCancel(context.Background())

	row := RegistryRow{
		id:                               0,
		ctx:                              ctx,
		cancelFn:                         cancelFn,
		finish:                           false,
		status:                           status,
		collectDataAfterDeploymentFinish: dr.collectDataAfterDeploymentFinish,
		DBSchema: DBSchema{
			Application:           appName,
			Cluster:               clusterName,
			Namespace:             namespace,
			CreationTimestamp:     deployTime,
			ReportTo:              reportTo,
			DeployBy:              deployBy,
			DeploymentDescription: DeploymentStatusDescriptionRunning,
			Resources: Resources{
				Deployments: make(map[string]*DeploymentData),
				Daemonsets:  make(map[string]*DaemonsetData),
			},
		},
	}

	dr.registryData[encodedID] = &row
	switch status {
	case DeploymentStatusRunning:
		dr.reporter.DeploymentStarted <- DeploymentReporter{
			To:       reportTo,
			DeployBy: deployBy,
			Name:     appName,
			URI:      row.GetURI(),
			Status:   status,
		}
	case DeploymentStatusDeleted:
		dr.reporter.DeploymentDeleted <- DeploymentReporter{
			To:       reportTo,
			DeployBy: deployBy,
			Name:     appName,
			URI:      row.GetURI(),
			Status:   status,
		}
	default:
		log.WithField("status", status).Info("Reporter status not supported")
	}

	log.WithFields(log.Fields{
		"application": appName,
		"deploy_by":   deployBy,
		"report_to":   reportTo,
		"namespace":   namespace,
		"cluster":     clusterName,
	}).Info("New application deployment started")

	go row.isFinish(dr.checkFinishDelay)
	return &row

}

// Get will return deployment row that exists in memory
func (dr *RegistryManager) Get(name, namespace string) *RegistryRow {

	encodedID := generateID(name, namespace)
	if row, found := dr.registryData[encodedID]; found {
		return row
	}
	return nil

}

// AddDeployment add new deployment under application
func (wbr *RegistryRow) AddDeployment(name, namespace string, labels map[string]string, desiredState int32, maxDeploymentTime int64) *DeploymentData {

	data := DeploymentData{
		Deployment: MetaData{
			Name:         name,
			Namespace:    namespace,
			Labels:       labels,
			DesiredState: desiredState,
		},
		Pods:                    make(map[string]DeploymenPod, 0),
		Replicaset:              make(map[string]Replicaset, 0),
		ProgressDeadlineSeconds: maxDeploymentTime,
	}
	wbr.DBSchema.Resources.Deployments[name] = &data

	log.WithFields(log.Fields{
		"application":   wbr.DBSchema.Application,
		"namespace":     wbr.DBSchema.Namespace,
		"deployment_id": name,
	}).Info("Deployment associated to application")

	return &data
}

// AddDaemonset add new daemonset under application
func (wbr *RegistryRow) AddDaemonset(name, namespace string, labels map[string]string, desiredState int32, maxDeploymentTime int64) *DaemonsetData {

	data := DaemonsetData{
		Metadata: MetaData{
			Name:         name,
			Namespace:    namespace,
			Labels:       labels,
			DesiredState: desiredState,
		},
		Pods:                    make(map[string]DeploymenPod, 0),
		ProgressDeadlineSeconds: maxDeploymentTime,
	}
	wbr.DBSchema.Resources.Daemonsets[name] = &data

	log.WithFields(log.Fields{
		"application":  wbr.DBSchema.Application,
		"namespace":    wbr.DBSchema.Namespace,
		"daemonset_id": name,
	}).Info("Daemonset associated to application")

	return &data
}

// GetURI will generate uri link for UI
func (wbr *RegistryRow) GetURI() string {
	return fmt.Sprintf("deployments/%s/%d", wbr.DBSchema.Application, wbr.DBSchema.CreationTimestamp)

}

// isDeploymentFinish will check for Deployment resource and see if it finished or errord due to timeout.
func (wbr *RegistryRow) isDeploymentFinish() (bool, error) {
	isFinished := false
	diff := time.Now().Sub(time.Unix(wbr.DBSchema.CreationTimestamp, 0)).Seconds()
	if len(wbr.DBSchema.Resources.Deployments) == 0 {
		isFinished = true
		return isFinished, nil
	}
	countOfRunningReplicas := 0
	var desiredStateCount int32
	var readyReplicasCount int32
	for _, deployment := range wbr.DBSchema.Resources.Deployments {
		desiredStateCount = desiredStateCount + deployment.Deployment.DesiredState
		for _, replica := range deployment.Replicaset {
			if replica.Status.Replicas > 0 {
				countOfRunningReplicas = countOfRunningReplicas + 1
			}
			readyReplicasCount = readyReplicasCount + replica.Status.ReadyReplicas
		}
		if deployment.ProgressDeadlineSeconds < int64(diff) {
			log.WithFields(log.Fields{
				"progress_deadline_seconds": deployment.ProgressDeadlineSeconds,
				"deploy_time":               diff,
				"application":               wbr.DBSchema.Application,
				"deployment":                deployment.Deployment.Name,
				"namespace":                 deployment.Deployment.Namespace,
			}).Error("Failed due to progress deadline")
			return isFinished, errors.New("ProgrogressDeadline has passed")
		}

	}
	log.WithFields(log.Fields{
		"application":          wbr.DBSchema.Application,
		"namespace":            wbr.DBSchema.Namespace,
		"replicaset_count":     countOfRunningReplicas,
		"desired_state_count":  desiredStateCount,
		"ready_replicas_count": readyReplicasCount,
		"count_deployments":    len(wbr.DBSchema.Resources.Deployments),
	}).Info("Deployment status")
	deploymentsNum := len(wbr.DBSchema.Resources.Deployments)
	if deploymentsNum == countOfRunningReplicas && desiredStateCount == readyReplicasCount || wbr.status == DeploymentStatusDeleted {
		log.WithFields(log.Fields{
			"application":          wbr.DBSchema.Application,
			"namespace":            wbr.DBSchema.Namespace,
			"replicaset_count":     countOfRunningReplicas,
			"desired_state_count":  desiredStateCount,
			"ready_replicas_count": readyReplicasCount,
		}).Info("Deployment was finished")

		// Wating few minutes to collect more event after deployment finished
		isFinished = true
		return isFinished, nil
	}
	return isFinished, nil
}

//isDaemonSetFinish  a DaemonSet is finished if: DesiredNumberScheduled == CurrentNumberScheduled AND DesiredNumberScheduled == UpdatedNumberScheduled
func (wbr *RegistryRow) isDaemonSetFinish() (bool, error) {
	isFinished := false
	if len(wbr.DBSchema.Resources.Daemonsets) == 0 {
		isFinished = true
		return isFinished, nil
	}
	totalDesiredPods := int32(0)
	totalUpdatedPodsOnNodes := int32(0)
	totalCurrentPods := int32(0)
	diff := time.Now().Sub(time.Unix(wbr.DBSchema.CreationTimestamp, 0)).Seconds()
	for _, daemonset := range wbr.DBSchema.Resources.Daemonsets {
		totalDesiredPods = totalDesiredPods + daemonset.Status.DesiredNumberScheduled
		totalUpdatedPodsOnNodes = totalUpdatedPodsOnNodes + daemonset.Status.DesiredNumberScheduled
		totalCurrentPods = totalCurrentPods + daemonset.Status.CurrentNumberScheduled

		if daemonset.ProgressDeadlineSeconds < int64(diff) {
			log.WithFields(log.Fields{
				"progress_deadline_seconds": daemonset.ProgressDeadlineSeconds,
				"deploy_time":               diff,
				"application":               wbr.DBSchema.Application,
				"daemonset":                 daemonset.Metadata.Name,
				"namespace":                 daemonset.Metadata.Namespace,
			}).Error("Failed due to progress deadline")
			return isFinished, errors.New("ProgrogressDeadline has passed")
		}
	}
	log.WithFields(log.Fields{
		"application":                   wbr.DBSchema.Application,
		"namespace":                     wbr.DBSchema.Namespace,
		"total_daemonsets_desired_pods": totalDesiredPods,
		"current_pods_count":            totalCurrentPods,
		"total_daemonsets":              len(wbr.DBSchema.Resources.Daemonsets),
	}).Debug("DaemonSet status")
	if totalDesiredPods == totalCurrentPods && totalDesiredPods == totalUpdatedPodsOnNodes || wbr.status == DeploymentStatusDeleted {
		log.WithFields(log.Fields{
			"application":                   wbr.DBSchema.Application,
			"namespace":                     wbr.DBSchema.Namespace,
			"total_daemonsets_desired_pods": totalDesiredPods,
			"current_pods_count":            totalCurrentPods,
			"total_daemonsets":              len(wbr.DBSchema.Resources.Daemonsets),
		}).Info("DaemonSet apply was finished")
		// Wating few minutes to collect more event after deployment finished
		isFinished = true
		return isFinished, nil
	}
	return isFinished, nil
}

// isFinish will check (by interval number) when the deployment finished by replicaset status
func (wbr *RegistryRow) isFinish(checkFinishDelay time.Duration) {
	log.WithFields(log.Fields{
		"application":      wbr.DBSchema.Application,
		"namespace":        wbr.DBSchema.Namespace,
		"deployment_count": len(wbr.DBSchema.Resources.Deployments),
		"daemonsets_count": len(wbr.DBSchema.Resources.Daemonsets),
		"applied_by":       len(wbr.DBSchema.DeployBy),
		"check_delay":      checkFinishDelay,
	}).Info("starting to watch on registry row")
	time.Sleep(checkFinishDelay)

	if wbr.status == DeploymentStatusDeleted {
		wbr.Stop(DeploymentStatusDeleted, DeploymentStatusDescriptionSuccessful)
		wbr.cancelFn()
		return
	}
	for {
		select {
		case <-time.After(time.Second * 2):
			if wbr.finish {
				return
			}
			isDepFinished, depErr := wbr.isDeploymentFinish()
			isDsFinished, dsErr := wbr.isDaemonSetFinish()
			if dsErr != nil || depErr != nil {
				wbr.Stop(DeploymentStatusFailed, DeploymentStatusDescriptionProgressDeadline)
				wbr.cancelFn()
				log.WithFields(log.Fields{
					"application":      wbr.DBSchema.Application,
					"namespace":        wbr.DBSchema.Namespace,
					"deployment_error": depErr,
					"daemonset_error":  dsErr,
				}).Error("isFinish function watch was errored")
				return
			} else if isDepFinished && isDsFinished {
				wbr.Stop(DeploymentSuccessful, DeploymentStatusDescriptionSuccessful)
				wbr.cancelFn()
			}
		case <-wbr.ctx.Done():
			log.WithFields(log.Fields{
				"application": wbr.DBSchema.Application,
				"namespace":   wbr.DBSchema.Namespace,
			}).Debug("isFinish function watch was stopped. Got ctx done signal")
			return

		}
	}
}

// Stop will marked the row as finish
func (wbr *RegistryRow) Stop(status DeploymentStatus, message DeploymentStatusDescription) {
	log.WithFields(log.Fields{
		"Name":   wbr.DBSchema.Application,
		"status": status,
	}).Debug("Marked as done")

	time.Sleep(wbr.collectDataAfterDeploymentFinish)
	wbr.DBSchema.DeploymentDescription = message
	wbr.finish = true
	wbr.status = status
}

// UpdateDeploymentStatus will update deployment status
func (dd *DeploymentData) UpdateDeploymentStatus(status appsV1.DeploymentStatus) {
	dd.Status = status
}

// UpdateDeploymentEvents will append events to deployment
func (dd *DeploymentData) UpdateDeploymentEvents(event EventMessages) {
	dd.DeploymentEvents = append(dd.DeploymentEvents, event)
}

// InitReplicaset create new list of replicaset
func (dd *DeploymentData) InitReplicaset(name string) {
	if _, found := dd.Replicaset[name]; !found {
		dd.Replicaset[name] = Replicaset{
			Events: &[]EventMessages{},
			Status: &appsV1.ReplicaSetStatus{},
		}
	}
}

// UpdateReplicasetEvents will append event to replicaset
func (dd *DeploymentData) UpdateReplicasetEvents(name string, event EventMessages) error {
	if _, found := dd.Replicaset[name]; !found {
		return errors.New("Replicaset not found")
	}
	*dd.Replicaset[name].Events = append(*dd.Replicaset[name].Events, event)

	return nil
}

// UpdateReplicasetStatus will update replicaset status
func (dd *DeploymentData) UpdateReplicasetStatus(name string, status appsV1.ReplicaSetStatus) error {
	if _, found := dd.Replicaset[name]; !found {
		return errors.New("Replicaset not found")
	}
	*dd.Replicaset[name].Status = status
	return nil
}

func NewPodToPods(pods map[string]DeploymenPod, pod *v1.Pod) error {
	if _, found := pods[pod.GetName()]; found {
		log.WithField("pod", pod.GetName()).Debug("Pod already exists in pod list")
		return errors.New("Pod already exists in pod list")
	}
	phase := string(pod.Status.Phase)
	pods[pod.GetName()] = DeploymenPod{
		Phase:  &phase,
		Events: &[]EventMessages{},
	}
	return nil
}

// NewPod will set new pod to deployment row
func (dd *DeploymentData) NewPod(pod *v1.Pod) error {
	return NewPodToPods(dd.Pods, pod)
}

// UpdatePodEvents will add event to pod events list
func UpdatePodEvents(pods map[string]DeploymenPod, podName string, event EventMessages) error {
	if _, found := pods[podName]; !found {
		log.WithField("pod", podName).Warn("Pod not exists in pod list")
		return errors.New("Pod not exists in pod list")
	}
	// Validate that we not inset duplicated events
	for _, saveEvent := range *pods[podName].Events {
		if saveEvent.Message == event.Message && saveEvent.Time == event.Time {
			return nil
		}
	}
	*pods[podName].Events = append(*pods[podName].Events, event)
	return nil
}

// UpdatePodEvents will set pod events
func (dd *DeploymentData) UpdatePodEvents(podName string, event EventMessages) error {
	return UpdatePodEvents(dd.Pods, podName, event)

}

// Get the deployment name
func (dd *DeploymentData) GetName() string {
	return dd.Deployment.Name
}

// UpdatePodStatus will change pod status
func UpdatePodStatus(pods map[string]DeploymenPod, pod *v1.Pod, status string) error {
	if _, found := pods[pod.GetName()]; !found {
		log.WithField("pod", pod.GetName()).Warn("Pod not exists in pod list")
		return errors.New("Pod not exists in pod list")
	}
	*pods[pod.GetName()].Phase = status
	return nil
}

// UpdatePod will set pod events to deployment
func (dd *DeploymentData) UpdatePod(pod *v1.Pod, status string) error {
	return UpdatePodStatus(dd.Pods, pod, status)

}

// UpdateApplyStatus will uppdate a daemonsets status
func (dsd *DaemonsetData) UpdateApplyStatus(status appsV1.DaemonSetStatus) {
	dsd.Status = status
}

// UpdateDaemonsetEvents will add event to a daemonset
func (dsd *DaemonsetData) UpdateDaemonsetEvents(event EventMessages) {
	dsd.DaemonsetEvents = append(dsd.DaemonsetEvents, event)
}

// UpdatePodEvents will set pod events
func (dsd *DaemonsetData) UpdatePodEvents(podName string, event EventMessages) error {
	return UpdatePodEvents(dsd.Pods, podName, event)
}

// UpdatePod will set pod events to daemonset
func (dsd *DaemonsetData) UpdatePod(pod *v1.Pod, status string) error {
	return UpdatePodStatus(dsd.Pods, pod, status)
}

// attach a new pod to the daemonset row
func (dsd *DaemonsetData) NewPod(pod *v1.Pod) error {
	return NewPodToPods(dsd.Pods, pod)
}

// GetName will get the daemonset name
func (dsd *DaemonsetData) GetName() string {
	return dsd.Metadata.Name
}

// save will save all the row list to the storage
func (dr *RegistryManager) save() {

	dr.saveLock.Lock()
	defer dr.saveLock.Unlock()

	var wg sync.WaitGroup
	wg.Add(len(dr.registryData))
	deleteRows := []string{}
	for key, data := range dr.registryData {
		go func(key string, data *RegistryRow, deleteRows *[]string) {
			defer wg.Done()
			if data.id == 0 {

				id, err := dr.storage.CreateApply(data, data.status)
				if err != nil {
					*deleteRows = append(*deleteRows, key)
					return
				}
				data.id = id
			} else {
				dr.storage.UpdateApply(data.id, data, data.status)
			}

			log.WithFields(log.Fields{
				"name": data.DBSchema.Application,
			}).Debug("Deployment was saved")

			if data.finish {

				if data.status != DeploymentStatusDeleted {
					dr.reporter.DeploymentFinished <- DeploymentReporter{
						To:       data.DBSchema.ReportTo,
						DeployBy: data.DBSchema.DeployBy,
						Name:     data.DBSchema.Application,
						URI:      data.GetURI(),
						Status:   data.status,
					}
				}

				*deleteRows = append(*deleteRows, key)
			}

		}(key, data, &deleteRows)

	}

	wg.Wait()

	for _, key := range deleteRows {
		delete(dr.registryData, key)
	}

}

// generateID will create a id for the deployment
func generateID(name, namespace string) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s-%s", name, namespace)))
}