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
	"context"
	"sort"
	"strings"
	"time"

	"github.com/koupleless/virtual-kubelet/tunnel"
	"github.com/koupleless/virtual-kubelet/virtual_kubelet/node"
	"github.com/koupleless/virtual-kubelet/virtual_kubelet/node/nodeutil"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/google/go-cmp/cmp"
	"github.com/koupleless/virtual-kubelet/common/tracker"
	"github.com/koupleless/virtual-kubelet/common/utils"
	"github.com/koupleless/virtual-kubelet/model"
	pkgerrors "github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
)

// Define the VPodProvider struct
var _ nodeutil.Provider = &VPodProvider{}
var _ node.PodNotifier = &VPodProvider{}

// VPodProvider is a struct that implements the nodeutil.Provider and virtual_kubelet.PodNotifier interfaces
type VPodProvider struct {
	Namespace string
	nodeName  string
	localIP   string
	client    client.Client
	cache     cache.Cache
	vPodStore *VPodStore // store the pod from provider

	tunnel tunnel.Tunnel

	port int

	notify func(pod *corev1.Pod)
}

// NotifyPods is a method of VPodProvider that sets the notify function
func (b *VPodProvider) NotifyPods(_ context.Context, cb func(*corev1.Pod)) {
	b.notify = cb
}

// NewVPodProvider is a function that creates a new VPodProvider instance
func NewVPodProvider(namespace, localIP, nodeName string, client client.Client, cache cache.Cache, tunnel tunnel.Tunnel) *VPodProvider {
	provider := &VPodProvider{
		Namespace: namespace,
		localIP:   localIP,
		nodeName:  nodeName,
		client:    client,
		cache:     cache,
		tunnel:    tunnel,
		vPodStore: NewVPodStore(),
	}

	return provider
}

// syncBizStatusToKube is a method of VPodProvider that synchronizes the status of related pods
func (b *VPodProvider) syncBizStatusToKube(ctx context.Context, bizStatusData model.BizStatusData) {
	logger := log.G(ctx)
	pod := b.vPodStore.GetPodByKey(bizStatusData.PodKey)
	if pod == nil {
		logger.Errorf("skip updating non-exist pod status for biz %s pod %s", bizStatusData.Key, bizStatusData.PodKey)
		return
	}

	// If the biz revision is set in the status data, update our tracking
	if bizStatusData.Revision > 0 {
		b.vPodStore.UpdateBizRevision(bizStatusData.Key, bizStatusData.Revision)
		logger.Infof("Updated revision for module %s to %d", bizStatusData.Key, bizStatusData.Revision)
	}

	podStatus, _ := b.GetPodStatus(ctx, pod, bizStatusData)

	podCopy := pod.DeepCopy()
	podStatus.DeepCopyInto(&podCopy.Status)
	b.vPodStore.PutPod(podCopy)
	b.notify(podCopy)
}

// SyncAllBizStatusToKube is a method of VPodProvider that synchronizes the information of all containers
func (b *VPodProvider) SyncAllBizStatusToKube(ctx context.Context, bizStatusDatas []model.BizStatusData) {
	bizKeyToBizStatusData := make(map[string]model.BizStatusData)
	for _, bizStatusData := range bizStatusDatas {
		bizKeyToBizStatusData[bizStatusData.Key] = bizStatusData

		// Update the revision tracking if the bizStatusData has a revision
		if bizStatusData.Revision > 0 {
			b.vPodStore.UpdateBizRevision(bizStatusData.Key, bizStatusData.Revision)
			log.G(ctx).Infof("Updated revision for module %s to %d from SyncAllBizStatusToKube",
				bizStatusData.Key, bizStatusData.Revision)
		}
	}

	pods := b.vPodStore.GetPods()
	// sort by create time
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].CreationTimestamp.UnixMilli() > pods[j].CreationTimestamp.UnixMilli()
	})

	// Initialize an empty slice to store updated container information
	toUpdateBizStatusDatas := make([]model.BizStatusData, 0)
	// Get the current time to use for change time
	now := time.Now()
	// Iterate through each pod
	for _, pod := range pods {
		// Get the key of the pod
		podKey := utils.GetPodKey(pod)
		// Iterate through each container in the pod
		for _, container := range pod.Spec.Containers {
			// Get the unique key of the container
			bizKey := utils.GetBizUniqueKey(&container)
			// Check if container information exists for the container key
			bizStatusData, has := bizKeyToBizStatusData[bizKey]
			// If container information does not exist, create a new deactivated instance
			if !has {
				bizStatusData = model.BizStatusData{
					Key:        bizKey,
					Name:       container.Name,
					PodKey:     podKey,
					State:      string(model.BizStateUnResolved),
					ChangeTime: now,
					Revision:   b.vPodStore.GetBizRevision(bizKey), // Use current revision if available
				}
			}

			namespace, name := utils.GetNameSpaceAndNameFromPodKey(bizStatusData.PodKey)
			pod := &corev1.Pod{}
			err := b.cache.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod)
			if err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to get pod %s from cache", podKey)
				continue
			}

			// Attempt to update the container status
			toUpdate := b.vPodStore.CheckContainerStatusNeedSync(pod, bizStatusData)
			// If the update was successful, add the container information to the updated list
			if toUpdate {
				toUpdateBizStatusDatas = append(toUpdateBizStatusDatas, bizStatusData)
			}

			log.G(ctx).Infof("container %s/%s need update: %v", podKey, bizKey, toUpdate)
		}
	}

	// Iterate through the provided container information and sync the related pod status
	for _, toUpdateBizStatus := range toUpdateBizStatusDatas {
		b.syncBizStatusToKube(ctx, toUpdateBizStatus)
	}
}

// SyncBizStatusToKube is a method of VPodProvider that synchronizes the information of a single container
func (b *VPodProvider) SyncBizStatusToKube(ctx context.Context, bizStatusData model.BizStatusData) {
	// Update the revision tracking if the bizStatusData has a revision
	if bizStatusData.Revision > 0 {
		b.vPodStore.UpdateBizRevision(bizStatusData.Key, bizStatusData.Revision)
		log.G(ctx).Infof("Updated revision for module %s to %d from SyncBizStatusToKube",
			bizStatusData.Key, bizStatusData.Revision)
	}

	namespace, name := utils.GetNameSpaceAndNameFromPodKey(bizStatusData.PodKey)
	pod := &corev1.Pod{}
	err := b.cache.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod)
	if err != nil {
		log.G(ctx).WithError(err).Error("Failed to get pod from cache")
		return
	}

	needSync := b.vPodStore.CheckContainerStatusNeedSync(pod, bizStatusData)
	log.G(ctx).Infof("container %s/%s need update: %v", bizStatusData.PodKey, bizStatusData.Key, needSync)
	if needSync {
		// only when container status updated, update related pod status
		b.syncBizStatusToKube(ctx, bizStatusData)
	}
}

// handleBizBatchStart is a method of VPodProvider that handles the start of a container
func (b *VPodProvider) handleBizBatchStart(ctx context.Context, pod *corev1.Pod, containers []corev1.Container) {
	podKey := utils.GetPodKey(pod)

	logger := log.G(ctx).WithField("podKey", podKey)
	logger.Info("HandleContainerStartOperation")

	labelMap := pod.Labels
	if labelMap == nil {
		labelMap = make(map[string]string)
	}

	// Get the pod revision from annotations or generate a new one
	podRevision := time.Now().UnixNano() // Default to current time in nanoseconds as revision
	if pod.Annotations != nil {
		if revStr, ok := pod.Annotations[model.AnnotationKeyOfPodRevision]; ok {
			var err error
			parsedRev, err := utils.ParseInt64(revStr)
			if err != nil {
				logger.WithError(err).Warnf("Failed to parse pod revision from annotation, using generated revision: %d", podRevision)
			} else {
				podRevision = parsedRev
			}
		}
	}

	for _, container := range containers {
		bizKey := utils.GetBizUniqueKey(&container)

		// Update the revision for this biz before starting
		// This ensures any newer module instance will have a higher revision
		currentRevision := b.vPodStore.GetBizRevision(bizKey)
		if podRevision <= currentRevision {
			// Make sure the new revision is higher than the current one
			podRevision = currentRevision + 1
		}

		// Update the revision in the store before starting the container
		b.vPodStore.UpdateBizRevision(bizKey, podRevision)

		logger.Infof("Starting module %s with revision %d", bizKey, podRevision)

		err := tracker.G().FuncTrack(labelMap[model.LabelKeyOfTraceID], model.TrackSceneVPodDeploy, model.TrackEventContainerStart, labelMap, func() (error, model.ErrorCode) {
			err := utils.CallWithRetry(ctx, func(_ int) (bool, error) {
				innerErr := b.tunnel.StartBiz(b.nodeName, podKey, &container)

				return innerErr != nil, innerErr
			}, nil)
			if err != nil {
				logger.WithError(err).WithField("containerKey", utils.GetContainerKey(podKey, container.Name)).Error("ContainerStartFailed")
			}
			return err, model.CodeSuccess
		})
		if err != nil {
			logger.WithError(err).WithField("containerKey", utils.GetContainerKey(podKey, container.Name)).Error("ContainerStartFailed")
		}
	}
}

// handleBizBatchStop is a method of VPodProvider that handles the shutdown of a container
func (b *VPodProvider) handleBizBatchStop(ctx context.Context, pod *corev1.Pod, containers []corev1.Container) {
	podKey := utils.GetPodKey(pod)

	logger := log.G(ctx).WithField("podKey", podKey)
	logger.Info("HandleContainerShutdownOperation")

	labelMap := pod.Labels
	if labelMap == nil {
		labelMap = make(map[string]string)
	}

	for _, container := range containers {
		bizKey := utils.GetBizUniqueKey(&container)
		podRevision := int64(0)

		// Get the pod revision from annotations if available
		if pod.Annotations != nil {
			if revStr, ok := pod.Annotations[model.AnnotationKeyOfPodRevision]; ok {
				var err error
				podRevision, err = utils.ParseInt64(revStr)
				if err != nil {
					logger.WithError(err).Warnf("Failed to parse pod revision from annotation, defaulting to 0")
				}
			}
		}

		// Check if the biz should be deleted based on revision comparison
		if !b.vPodStore.ShouldDeleteBiz(bizKey, podRevision) {
			logger.Infof("Skipping deletion of module %s as its revision %d is less than current revision %d",
				bizKey, podRevision, b.vPodStore.GetBizRevision(bizKey))
			continue
		}

		err := tracker.G().FuncTrack(labelMap[model.LabelKeyOfTraceID], model.TrackSceneVPodDeploy, model.TrackEventContainerShutdown, labelMap, func() (error, model.ErrorCode) {
			err := utils.CallWithRetry(ctx, func(_ int) (bool, error) {
				innerErr := b.tunnel.StopBiz(b.nodeName, podKey, &container)

				return innerErr != nil, innerErr
			}, nil)
			if err != nil {
				return err, model.CodeContainerStopFailed
			}
			return nil, model.CodeSuccess
		})
		if err != nil {
			logger.WithError(err).WithField("containerKey", utils.GetContainerKey(podKey, container.Name)).Error("ContainerShutdownFailed")
		}
	}
}

// CreatePod is a method of VPodProvider that creates a pod
func (b *VPodProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.G(ctx).WithField("podKey", utils.GetPodKey(pod))
	logger.Info("CreatePodStarted")

	// update the baseline info so the async handle logic can see them first
	podCopy := pod.DeepCopy()
	b.vPodStore.PutPod(podCopy)
	b.handleBizBatchStart(ctx, podCopy, podCopy.Spec.Containers)
	b.notify(podCopy)
	return nil
}

// UpdatePod is a method of VPodProvider that updates a pod
func (b *VPodProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	podKey := utils.GetPodKey(pod)
	logger := log.G(ctx).WithField("podKey", podKey)
	logger.Info("UpdatePodStarted")

	newPod := pod.DeepCopy()

	// check pod deletion timestamp
	if newPod.ObjectMeta.DeletionTimestamp != nil {
		// skip deleted pod
		return nil
	}

	oldPod := b.vPodStore.GetPodByKey(podKey).DeepCopy()
	if oldPod == nil {
		return pkgerrors.Errorf("pod %s not found when updating", podKey)
	}

	// Get the new revision from annotations
	newRevision := int64(0)
	if newPod.Annotations != nil {
		if revStr, ok := newPod.Annotations[model.AnnotationKeyOfPodRevision]; ok {
			var err error
			newRevision, err = utils.ParseInt64(revStr)
			if err != nil {
				logger.WithError(err).Warnf("Failed to parse pod revision from annotation, defaulting to 0")
			}
		}
	}

	newContainerMap := make(map[string]corev1.Container)
	oldContainerMap := make(map[string]corev1.Container)
	for _, container := range newPod.Spec.Containers {
		newContainerMap[container.Name] = container
	}
	for _, container := range oldPod.Spec.Containers {
		oldContainerMap[container.Name] = container
	}

	shouldStopContainers := make([]corev1.Container, 0)
	shouldStartContainers := make([]corev1.Container, 0)
	// find the container that updated in new pod
	for name, oldContainer := range oldContainerMap {
		if newContainer, has := newContainerMap[name]; has && !cmp.Equal(newContainer, oldContainer) {
			shouldStopContainers = append(shouldStopContainers, oldContainer)
			shouldStartContainers = append(shouldStartContainers, newContainer)
		}
	}
	// find the new container that not existed in old pod
	for name, newContainer := range newContainerMap {
		if _, has := oldContainerMap[name]; !has {
			shouldStartContainers = append(shouldStartContainers, newContainer)
		}
	}

	if len(shouldStopContainers) > 0 {
		b.handleBizBatchStop(ctx, oldPod, shouldStopContainers)
	}

	// Update the revision for all containers in the new pod
	for _, container := range newPod.Spec.Containers {
		bizKey := utils.GetBizUniqueKey(&container)
		if bizKey != "" {
			b.vPodStore.UpdateBizRevision(bizKey, newRevision)
			logger.Infof("Updated revision for module %s to %d", bizKey, newRevision)
		}
	}

	b.vPodStore.PutPod(newPod.DeepCopy())

	if len(shouldStartContainers) == 0 {
		b.notify(newPod)
		return nil
	}

	// only start new containers and changed containers
	tracker.G().Eventually(pod.Labels[model.LabelKeyOfTraceID], model.TrackSceneVPodDeploy, model.TrackEventVPodUpdate, pod.Labels, model.CodeContainerStartTimeout, func(context.Context) (bool, error) {
		podFromKubernetes := &corev1.Pod{}
		err := b.client.Get(ctx, client.ObjectKey{
			Namespace: pod.Namespace,
			Name:      pod.Name,
		}, podFromKubernetes)
		if err != nil {
			logger.WithError(err).Error("Failed to get pod from k8s")
			// should failed if can't get pod from k8s
			if errors.IsNotFound(err) {
				// stop retry and no need to start new containers
				return false, err
			}
			return false, nil
		}

		nameToContainerStatus := make(map[string]corev1.ContainerStatus)
		for _, containerStatus := range podFromKubernetes.Status.ContainerStatuses {
			nameToContainerStatus[containerStatus.Name] = containerStatus
		}

		for _, shouldUpdateContainer := range shouldStopContainers {
			if status, has := nameToContainerStatus[shouldUpdateContainer.Name]; has && status.State.Terminated == nil {
				return false, nil
			}
		}
		return true, nil
	}, time.Minute, time.Second, func() {
		b.handleBizBatchStart(ctx, newPod, shouldStopContainers)
	}, func() {
		logger.Error("stop old containers timeout, not start new containers")
	})

	b.notify(newPod)
	return nil
}

// DeletePod is a method of VPodProvider that deletes a pod
func (b *VPodProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	podKey := utils.GetPodKey(pod)
	logger := log.G(ctx).WithField("podKey", podKey)
	logger.Info("DeletePodStarted")
	if pod == nil {
		// this should never happen
		return nil
	}

	// delete from curr provider
	b.vPodStore.DeletePod(podKey)
	// Handle stopping the module containers with revision check
	b.handleBizBatchStop(ctx, pod, pod.Spec.Containers)
	b.notify(pod)
	return nil
}

// GetPod is a method of VPodProvider that gets a pod
// This method is simply used to return the observed defaultPod by local
//
//	so the outer control loop can call CreatePod / UpdatePod / DeletePod accordingly
//	just return the defaultPod from the local store
func (b *VPodProvider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	return b.vPodStore.GetPodByKey(namespace + "/" + name), nil
}

// GetPodStatus is a method of VPodProvider that gets the status of a pod
// This will be called repeatedly by virtual kubelet framework to get the defaultPod status
// we should query the actual runtime info and convert them in to V1PodStatus accordingly
func (b *VPodProvider) GetPodStatus(ctx context.Context, pod *corev1.Pod, bizStatus model.BizStatusData) (*corev1.PodStatus, error) {
	podStatus := &corev1.PodStatus{}
	// check pod status
	bizJarContainerCount := 0
	readyBizJarContainerCount := 0
	terminatedBizJarContainerCount := 0
	notReadyBizJarContainerCount := 0
	notInitedBizJarContainerCount := 0

	podStatus.PodIP = b.localIP
	podStatus.PodIPs = []corev1.PodIP{{IP: b.localIP}}
	podStatus.ContainerStatuses = make([]corev1.ContainerStatus, 0)

	nameToContainerStatus := make(map[string]*corev1.ContainerStatus)
	for _, cs := range pod.Status.ContainerStatuses {
		nameToContainerStatus[cs.Name] = &cs
	}

	// TODO: check all containers status only biz jar container
	for _, container := range pod.Spec.Containers {
		// only check biz jar container
		if !strings.Contains(container.Image, ".jar") {
			continue
		}
		containerStatus, err := utils.ConvertBizStatusToContainerStatus(&container, nameToContainerStatus[container.Name], &bizStatus)
		if err != nil || containerStatus == nil {
			log.G(ctx).Errorf("can't convert biz status to container status for container %s", utils.GetContainerKey(utils.GetPodKey(pod), container.Name))
			return nil, err
		} else {
			podStatus.ContainerStatuses = append(podStatus.ContainerStatuses, *containerStatus)
		}

		bizJarContainerCount++
		if containerStatus.Ready {
			readyBizJarContainerCount++
		} else if containerStatus.State.Terminated != nil {
			terminatedBizJarContainerCount++
		} else if containerStatus.State.Waiting != nil || containerStatus.State.Running != nil {
			notReadyBizJarContainerCount++
		} else {
			// not init yet
			notInitedBizJarContainerCount++
		}
	}

	podStatus.Phase = corev1.PodPending

	if bizJarContainerCount == 0 || bizJarContainerCount == terminatedBizJarContainerCount {
		// if no biz jar container or all biz jar container terminated, pod is terminated
		podStatus.Phase = corev1.PodSucceeded
		podStatus.Conditions = []corev1.PodCondition{
			{
				Type:          "Ready",
				Status:        corev1.ConditionFalse,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
			{
				Type:          "ContainersReady",
				Status:        corev1.ConditionFalse,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
		}
		return podStatus, nil
	} else if notInitedBizJarContainerCount == bizJarContainerCount {
		podStatus.Phase = corev1.PodPending
		podStatus.Conditions = []corev1.PodCondition{
			{
				Type:          "Ready",
				Status:        corev1.ConditionFalse,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
			{
				Type:          "ContainersReady",
				Status:        corev1.ConditionFalse,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
		}
	} else if bizJarContainerCount == readyBizJarContainerCount {
		// all biz jar container is ready
		podStatus.Phase = corev1.PodRunning
		podStatus.Conditions = []corev1.PodCondition{
			{
				Type:          "Ready",
				Status:        corev1.ConditionTrue,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
			{
				Type:          "ContainersReady",
				Status:        corev1.ConditionTrue,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
		}
	} else if notReadyBizJarContainerCount > 0 || (readyBizJarContainerCount > 0 && readyBizJarContainerCount < bizJarContainerCount) {
		podStatus.Phase = corev1.PodRunning
		podStatus.Conditions = []corev1.PodCondition{
			{
				Type:          "Ready",
				Status:        corev1.ConditionFalse,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
			{
				Type:          "ContainersReady",
				Status:        corev1.ConditionFalse,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
		}
	} else {
		podStatus.Phase = corev1.PodPending
		podStatus.Conditions = []corev1.PodCondition{
			{
				Type:          "Ready",
				Status:        corev1.ConditionFalse,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
			{
				Type:          "ContainersReady",
				Status:        corev1.ConditionFalse,
				LastProbeTime: metav1.NewTime(time.Now()),
			},
		}
	}

	return podStatus, nil
}

func (b *VPodProvider) GetPods(_ context.Context) ([]*corev1.Pod, error) {
	return b.vPodStore.GetPods(), nil
}
