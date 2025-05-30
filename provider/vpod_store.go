/**
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package provider

import (
	"strings"
	"sync"
	"time"

	"github.com/koupleless/virtual-kubelet/common/utils"
	"github.com/koupleless/virtual-kubelet/model"
	corev1 "k8s.io/api/core/v1"
)

// VPodStore provides in-memory runtime information.
type VPodStore struct {
	sync.RWMutex // This mutex is used for thread-safe access to the store.

	podKeyToPod      map[string]*corev1.Pod // Maps pod keys to their corresponding pods from provider
	bizKeyToRevision map[string]int64       // Maps bizKey to its latest revision number
}

func NewVPodStore() *VPodStore {
	return &VPodStore{
		RWMutex:          sync.RWMutex{},
		podKeyToPod:      make(map[string]*corev1.Pod),
		bizKeyToRevision: make(map[string]int64),
	}
}

// PutPod function updates or adds a pod to the VPodStore.
func (r *VPodStore) PutPod(pod *corev1.Pod) {
	r.Lock()
	defer r.Unlock()

	podKey := utils.GetPodKey(pod)

	// create or update
	r.podKeyToPod[podKey] = pod
}

// DeletePod function removes a pod from the VPodStore.
func (r *VPodStore) DeletePod(podKey string) {
	r.Lock()
	defer r.Unlock()

	delete(r.podKeyToPod, podKey)
}

// GetPodByKey function retrieves a pod by its key.
func (r *VPodStore) GetPodByKey(podKey string) *corev1.Pod {
	r.RLock()
	defer r.RUnlock()
	return r.podKeyToPod[podKey]
}

// GetPods function retrieves all pods in the VPodStore.
func (r *VPodStore) GetPods() []*corev1.Pod {
	r.RLock()
	defer r.RUnlock()

	ret := make([]*corev1.Pod, 0, len(r.podKeyToPod))
	for _, pod := range r.podKeyToPod {
		ret = append(ret, pod)
	}
	return ret
}

// UpdateBizRevision updates the revision for a specific bizKey
func (r *VPodStore) UpdateBizRevision(bizKey string, revision int64) {
	r.Lock()
	defer r.Unlock()

	r.bizKeyToRevision[bizKey] = revision
}

// GetBizRevision gets the current revision for a specific bizKey
func (r *VPodStore) GetBizRevision(bizKey string) int64 {
	r.RLock()
	defer r.RUnlock()

	return r.bizKeyToRevision[bizKey]
}

// ShouldDeleteBiz checks if a biz should be deleted based on its revision
// Returns true if delete should proceed, false otherwise
func (r *VPodStore) ShouldDeleteBiz(bizKey string, revision int64) bool {
	r.RLock()
	defer r.RUnlock()

	currentRevision, exists := r.bizKeyToRevision[bizKey]
	// If it doesn't exist in our tracking or the deletion revision is greater/equal, allow deletion
	return !exists || revision >= currentRevision
}

func (r *VPodStore) CheckContainerStatusNeedSync(pod *corev1.Pod, bizStatusData model.BizStatusData) bool {
	r.Lock()
	defer r.Unlock()

	var matchedStatus *corev1.ContainerStatus
	var matchedContainer *corev1.Container
	for _, status := range pod.Status.ContainerStatuses {
		if (status.Name == bizStatusData.Name) && strings.Contains(status.Image, ".jar") {
			matchedStatus = &status
		}
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == bizStatusData.Name {
			matchedContainer = &container
		}
	}

	// If no matching container is found, return false
	if matchedContainer == nil {
		return false
	}

	// the earliest change time of the container status when no time
	oldChangeTime := time.Time{}
	if matchedStatus != nil {
		if matchedStatus.State.Running != nil {
			oldChangeTime = matchedStatus.State.Running.StartedAt.Time
		}
		if matchedStatus.State.Terminated != nil {
			oldChangeTime = matchedStatus.State.Terminated.FinishedAt.Time
		}
		if matchedStatus.State.Waiting != nil && pod.Status.Conditions != nil && len(pod.Status.Conditions) > 0 {
			oldChangeTime = pod.Status.Conditions[0].LastTransitionTime.Time
		}
	}

	// Update the revision tracking for this biz container
	bizKey := utils.GetBizUniqueKey(matchedContainer)
	if bizKey != "" && bizStatusData.Revision > 0 {
		r.bizKeyToRevision[bizKey] = bizStatusData.Revision
	}

	// 优化 bizStatusData.ChangeTime，只有 bizState 变化的时间才需要更新
	// 由于 k8s 里记录的时间只到秒，需要先对齐时间精度再进行比较
	if bizStatusData.ChangeTime.Second() > oldChangeTime.Second() {
		return true
	} else {
		return false
	}
}
