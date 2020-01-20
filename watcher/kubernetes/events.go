package kuberneteswatcher

import (
	"context"
	"statusbay/serverutil"
	"time"

	log "github.com/sirupsen/logrus"

	v1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// WatchEvents struct
type WatchEvents struct {

	// ListOptions include the query options for event
	ListOptions metaV1.ListOptions

	// Event namespace
	Namespace string

	// Option to close the channel when context to done
	Ctx context.Context
}

// EventsManager defined pods manager struct
type EventsManager struct {
	client kubernetes.Interface
}

// NewEventsManager create new pods instance
func NewEventsManager(kubernetesClientset kubernetes.Interface) *EventsManager {
	return &EventsManager{
		client: kubernetesClientset,
	}
}

// Serve will start listening on pods request
func (em *EventsManager) Serve() serverutil.StopFunc {

	ctx, cancelFn := context.WithCancel(context.Background())
	stopped := make(chan bool)
	go func() {
		for {
			select {
			case <-ctx.Done():
				log.Warn("Event Manager has been shut down")
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

// Watch will start watch resource events
func (em *EventsManager) Watch(watchData WatchEvents) <-chan EventMessages {

	responses := make(chan EventMessages, 0)

	log.WithFields(log.Fields{
		"list_option": watchData.ListOptions.String(),
		"namespace":   watchData.Namespace,
	}).Debug("Watch event started")

	go func() {
		watcher, err := em.client.CoreV1().Events("").Watch(watchData.ListOptions)
		if err != nil {
			log.WithError(err).WithFields(log.Fields{
				"list_option": watchData.ListOptions.String(),
				"namespace":   watchData.Namespace,
			}).Error("Failed to watch on events")
			return
		}
		for {
			select {
			case event, watch := <-watcher.ResultChan():
				if !watch {
					log.WithFields(log.Fields{
						"list_options": watchData.ListOptions.String(),
						"timeout":      watchData.ListOptions.TimeoutSeconds,
					}).Warn("Stop watching on events, got timeout")
					return
				}

				eventData, ok := event.Object.(*v1.Event)
				if !ok {
					log.WithFields(log.Fields{
						"list_option": watchData.ListOptions.String(),
						"namespace":   watchData.Namespace,
						"object":      event.Object,
					}).Warn("Failed to parse event object")
					continue
				}
				diff := time.Now().Sub(eventData.GetCreationTimestamp().Time).Seconds()
				// TODO:: move to configuration settup
				if diff < 30 {

					responses <- EventMessages{
						Message:             eventData.Message,
						Time:                eventData.GetCreationTimestamp().Time.UnixNano(),
						Action:              eventData.Action,
						ReportingController: eventData.ReportingController,
					}
				} else {
					log.WithFields(log.Fields{
						"Message":     eventData.Message,
						"time":        eventData.GetCreationTimestamp(),
						"list_option": watchData.ListOptions.String(),
						"namespace":   watchData.Namespace,
						"object":      event.Object,
					}).Debug("Event to old")
				}

			case <-watchData.Ctx.Done():
				log.WithFields(log.Fields{
					"list_options": watchData.ListOptions.String(),
				}).Debug("Stop events watch. Got ctx done signal")
				watcher.Stop()
				return
			}
		}
	}()
	return responses

}