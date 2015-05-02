/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package kubelet

import (
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/record"
	kubecontainer "github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/container"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/types"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/golang/glog"
)

type syncPodFnType func(*api.Pod, *api.Pod, kubecontainer.Pod) error

type podWorkers struct {
	// Protects all per worker fields.
	podLock sync.Mutex

	// Tracks all running per-pod goroutines - per-pod goroutine will be
	// processing updates received through its corresponding channel.
	podUpdates map[types.UID]chan workUpdate
	// Track the current state of per-pod goroutines.
	// Currently all update request for a given pod coming when another
	// update of this pod is being processed are ignored.
	isWorking map[types.UID]bool
	// Tracks the last undelivered work item for this pod - a work item is
	// undelivered if it comes in while the worker is working.
	lastUndeliveredWorkUpdate map[types.UID]workUpdate
	// runtimeCache is used for listing running containers.
	runtimeCache kubecontainer.RuntimeCache

	// This function is run to sync the desired stated of pod.
	// NOTE: This function has to be thread-safe - it can be called for
	// different pods at the same time.
	syncPodFn syncPodFnType

	// The EventRecorder to use
	recorder record.EventRecorder
}

type workUpdate struct {
	// The pod state to reflect.
	pod *api.Pod

	// The mirror pod of pod; nil if it does not exist.
	mirrorPod *api.Pod

	// Function to call when the update is complete.
	updateCompleteFn func()
}

func newPodWorkers(runtimeCache kubecontainer.RuntimeCache, syncPodFn syncPodFnType,
	recorder record.EventRecorder) *podWorkers {
	return &podWorkers{
		podUpdates:                map[types.UID]chan workUpdate{},
		isWorking:                 map[types.UID]bool{},
		lastUndeliveredWorkUpdate: map[types.UID]workUpdate{},
		runtimeCache:              runtimeCache,
		syncPodFn:                 syncPodFn,
		recorder:                  recorder,
	}
}

func (p *podWorkers) managePodLoop(podUpdates <-chan workUpdate) {
	var minRuntimeCacheTime time.Time
	for newWork := range podUpdates {
		func() {
			defer p.checkForUpdates(newWork.pod.UID, newWork.updateCompleteFn)
			// We would like to have the state of Docker from at least the moment
			// when we finished the previous processing of that pod.
			if err := p.runtimeCache.ForceUpdateIfOlder(minRuntimeCacheTime); err != nil {
				glog.Errorf("Error updating docker cache: %v", err)
				return
			}
			pods, err := p.runtimeCache.GetPods()
			if err != nil {
				glog.Errorf("Error getting pods while syncing pod: %v", err)
				return
			}

			err = p.syncPodFn(newWork.pod, newWork.mirrorPod,
				kubecontainer.Pods(pods).FindPodByID(newWork.pod.UID))
			if err != nil {
				glog.Errorf("Error syncing pod %s, skipping: %v", newWork.pod.UID, err)
				p.recorder.Eventf(newWork.pod, "failedSync", "Error syncing pod, skipping: %v", err)
				return
			}
			minRuntimeCacheTime = time.Now()

			newWork.updateCompleteFn()
		}()
	}
}

// Apply the new setting to the specified pod. updateComplete is called when the update is completed.
func (p *podWorkers) UpdatePod(pod *api.Pod, mirrorPod *api.Pod, updateComplete func()) {
	uid := pod.UID
	var podUpdates chan workUpdate
	var exists bool

	p.podLock.Lock()
	defer p.podLock.Unlock()
	if podUpdates, exists = p.podUpdates[uid]; !exists {
		// We need to have a buffer here, because checkForUpdates() method that
		// puts an update into channel is called from the same goroutine where
		// the channel is consumed. However, it is guaranteed that in such case
		// the channel is empty, so buffer of size 1 is enough.
		podUpdates = make(chan workUpdate, 1)
		p.podUpdates[uid] = podUpdates
		go func() {
			defer util.HandleCrash()
			p.managePodLoop(podUpdates)
		}()
	}
	if !p.isWorking[pod.UID] {
		p.isWorking[pod.UID] = true
		podUpdates <- workUpdate{
			pod:              pod,
			mirrorPod:        mirrorPod,
			updateCompleteFn: updateComplete,
		}
	} else {
		p.lastUndeliveredWorkUpdate[pod.UID] = workUpdate{
			pod:              pod,
			mirrorPod:        mirrorPod,
			updateCompleteFn: updateComplete,
		}
	}
}

func (p *podWorkers) ForgetNonExistingPodWorkers(desiredPods map[types.UID]empty) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	for key, channel := range p.podUpdates {
		if _, exists := desiredPods[key]; !exists {
			close(channel)
			delete(p.podUpdates, key)
			// If there is an undelivered work update for this pod we need to remove it
			// since per-pod goroutine won't be able to put it to the already closed
			// channel when it finish processing the current work update.
			if _, cached := p.lastUndeliveredWorkUpdate[key]; cached {
				delete(p.lastUndeliveredWorkUpdate, key)
			}
		}
	}
}

func (p *podWorkers) checkForUpdates(uid types.UID, updateComplete func()) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if workUpdate, exists := p.lastUndeliveredWorkUpdate[uid]; exists {
		p.podUpdates[uid] <- workUpdate
		delete(p.lastUndeliveredWorkUpdate, uid)
	} else {
		p.isWorking[uid] = false
	}
}
