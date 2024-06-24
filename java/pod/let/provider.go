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

package let

import (
	"context"
	"errors"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"

	"github.com/koupleless/arkctl/v1/service/ark"
	"github.com/koupleless/module-controller/common/queue"
	"github.com/koupleless/module-controller/java/common"
	"github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/api/statsv1alpha1"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
)

var _ nodeutil.Provider = &BaseProvider{}

const LOOP_BACK_IP = "127.0.0.1"

type BaseProvider struct {
	namespace string

	localIP                 string
	arkService              ark.Service
	modelUtils              common.ModelUtils
	runtimeInfoStore        *RuntimeInfoStore
	installOperationQueue   *queue.Queue
	uninstallOperationQueue *queue.Queue

	port int
}

func NewBaseProvider(namespace string) *BaseProvider {
	provider := &BaseProvider{
		namespace:        namespace,
		localIP:          LOOP_BACK_IP, //todo: get the local ip
		arkService:       ark.BuildService(context.Background()),
		modelUtils:       common.ModelUtils{},
		runtimeInfoStore: NewRuntimeInfoStore(),
		port:             1238,
	}

	provider.installOperationQueue = queue.New(
		workqueue.DefaultControllerRateLimiter(),
		"bizInstallOperationQueue",
		provider.handleInstallOperation,
		// todo: more complicated retry logic
		func(ctx context.Context, key string, timesTried int, originallyAdded time.Time, err error) (*time.Duration, error) {
			duration := time.Millisecond * 100
			return &duration, nil
		},
	)

	provider.uninstallOperationQueue = queue.New(
		workqueue.DefaultControllerRateLimiter(),
		"bizUninstallOperationQueue",
		provider.handleUnInstallOperation,
		// todo: more complicated retry logic
		func(ctx context.Context, key string, timesTried int, originallyAdded time.Time, err error) (*time.Duration, error) {
			duration := time.Millisecond * 100
			return &duration, nil
		},
	)

	return provider
}

func (b *BaseProvider) Run(ctx context.Context) {
	go b.installOperationQueue.Run(ctx, 1)
	go b.uninstallOperationQueue.Run(ctx, 1)
}

func (b *BaseProvider) queryAllBiz(ctx context.Context) ([]ark.ArkBizInfo, error) {
	resp, err := b.arkService.QueryAllBiz(ctx, ark.QueryAllArkBizRequest{
		HostName: LOOP_BACK_IP,
		Port:     b.port,
	})
	if err != nil {
		log.G(ctx).WithError(err).Error("QueryAllBizFailed")
		return nil, err
	}

	if resp.Code != "SUCCESS" {
		err = errors.New(resp.Message)
		log.G(ctx).WithError(err).Error("QueryAllBizFailed")
		return nil, err
	}

	return resp.Data, nil
}

func (b *BaseProvider) queryBiz(ctx context.Context, bizIdentity string) (*ark.ArkBizInfo, error) {
	infos, err := b.queryAllBiz(ctx)
	if err != nil {
		return nil, err
	}

	for _, info := range infos {
		infoIdentity := b.modelUtils.GetBizIdentityFromBizInfo(&info)
		if infoIdentity == bizIdentity {
			return &info, nil
		}
	}

	return nil, nil
}

func (b *BaseProvider) installBiz(ctx context.Context, bizModel *ark.BizModel) error {
	if err := b.arkService.InstallBiz(ctx, ark.InstallBizRequest{
		BizModel: *bizModel,
		TargetContainer: ark.ArkContainerRuntimeInfo{
			RunType:    ark.ArkContainerRunTypeLocal,
			Coordinate: LOOP_BACK_IP,
			Port:       &b.port,
		},
	}); err != nil {
		log.G(ctx).WithError(err).Info("InstallBizFailed")
		return err
	}
	return nil
}

func (b *BaseProvider) unInstallBiz(ctx context.Context, bizModel *ark.BizModel) error {
	if err := b.arkService.UnInstallBiz(ctx, ark.UnInstallBizRequest{
		BizModel: *bizModel,
		TargetContainer: ark.ArkContainerRuntimeInfo{
			RunType:    ark.ArkContainerRunTypeLocal,
			Coordinate: LOOP_BACK_IP,
			Port:       &b.port,
		},
	}); err != nil {
		log.G(ctx).WithError(err).Info("UnInstallBizFailed")
		return err
	}
	return nil
}

func (b *BaseProvider) handleInstallOperation(ctx context.Context, bizIdentity string) error {
	logger := log.G(ctx).WithField("bizIdentity", bizIdentity)
	logger.Info("HandleBizInstallOperationStarted")

	bizModel := b.runtimeInfoStore.GetBizModel(bizIdentity)
	if bizModel == nil {
		// for installation, this should never happen
		logger.Error("Installing non-existent defaultPod")
		return errors.New("installing non-existent defaultPod")
	}
	bizInfo, err := b.queryBiz(ctx, bizIdentity)
	if err != nil {
		logger.WithError(err).Error("QueryBizFailed")
		return err
	}

	if bizInfo != nil && bizInfo.BizState == "ACTIVATED" {
		logger.Info("BizAlreadyActivated")
		return nil
	}

	if bizInfo != nil && bizInfo.BizState == "RESOLVED" {
		// process concurrent install operation
		logger.Info("BizInstalling")
		return nil
	}

	if bizInfo != nil && bizInfo.BizState != "DEACTIVATED" {
		// todo: support retry accordingly
		//       we should check the related defaultPod failed strategy and retry accordingly
		logger.Error("BizInstalledButNotActivated")
		return errors.New("BizInstalledButNotActivated")
	}

	if err := b.installBiz(ctx, bizModel); err != nil {
		logger.WithError(err).Error("InstallBizFailed")
		return err
	}

	logger.Info("HandleBizInstallOperationFinished")
	return nil
}

func (b *BaseProvider) handleUnInstallOperation(ctx context.Context, bizIdentity string) error {
	logger := log.G(ctx).WithField("bizIdentity", bizIdentity)
	logger.Info("HandleUnInstallOperationStarted")

	bizInfo, err := b.queryBiz(ctx, bizIdentity)
	if err != nil {
		logger.WithError(err).Error("QueryBizFailed")
		return err
	}

	if bizInfo != nil {
		// local installed, call uninstall
		if err := b.unInstallBiz(ctx, &ark.BizModel{
			BizName:    bizInfo.BizName,
			BizVersion: bizInfo.BizVersion,
		}); err != nil {
			logger.WithError(err).Error("UnInstallBizFailed")
			return err
		}
	}

	logger.Info("HandleBizUninstallOperationFinished")
	return nil
}

// CreatePod directly install a biz bundle to base
func (b *BaseProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.G(ctx).WithField("podKey", b.modelUtils.GetPodKey(pod))
	logger.Info("CreatePodStarted")

	// update the baseline info so the async handle logic can see them first
	b.runtimeInfoStore.PutPod(pod)
	bizModels := b.modelUtils.GetBizModelsFromCoreV1Pod(pod)
	for _, bizModel := range bizModels {
		b.installOperationQueue.Enqueue(ctx, b.modelUtils.GetBizIdentityFromBizModel(bizModel))
		logger.WithField("bizName", bizModel.BizName).WithField("bizVersion", bizModel.BizVersion).Info("ItemEnqueued")
	}

	return nil
}

// UpdatePod uninstall then install
func (b *BaseProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	podKey := b.modelUtils.GetPodKey(pod)
	logger := log.G(ctx).WithField("podKey", podKey)
	logger.Info("UpdatePodStarted")

	oldModels := b.runtimeInfoStore.GetRelatedBizModels(podKey)

	// delete old models
	for _, oldModel := range oldModels {
		b.uninstallOperationQueue.Enqueue(ctx, b.modelUtils.GetBizIdentityFromBizModel(oldModel))
		logger.WithField("bizName", oldModel.BizName).WithField("bizVersion", oldModel.BizVersion).Info("ItemEnqueued")
	}

	newModels := b.modelUtils.GetBizModelsFromCoreV1Pod(pod)

	b.runtimeInfoStore.PutPod(pod)

	// check pod deletion timestamp
	if pod.ObjectMeta.DeletionTimestamp == nil {
		// not in deletion, install new models
		for _, newModel := range newModels {
			b.installOperationQueue.Enqueue(ctx, b.modelUtils.GetBizIdentityFromBizModel(newModel))
			logger.WithField("bizName", newModel.BizName).WithField("bizVersion", newModel.BizVersion).Info("ItemEnqueued")
		}
	}

	return nil
}

// DeletePod directly uninstall biz  from base
func (b *BaseProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	podKey := b.modelUtils.GetPodKey(pod)
	logger := log.G(ctx).WithField("podKey", podKey)
	logger.Info("DeletePodStarted")

	bizModels := b.runtimeInfoStore.GetRelatedBizModels(podKey)
	b.runtimeInfoStore.PutPod(pod)
	for _, bizModel := range bizModels {
		bizIdentity := b.modelUtils.GetBizIdentityFromBizModel(bizModel)
		b.uninstallOperationQueue.Enqueue(ctx, bizIdentity)
		logger.WithField("bizName", bizModel.BizName).WithField("bizVersion", bizModel.BizVersion).Info("ItemEnqueued")
	}

	return nil
}

// GetPod this method is simply used to return the observed defaultPod by local
//
//	so the outer control loop can call CreatePod / UpdatePod / DeletePod accordingly
//	just return the defaultPod from the local store
func (b *BaseProvider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	return b.runtimeInfoStore.GetPodByKey(namespace + "/" + name), nil
}

// GetPodStatus this will be called repeatedly by virtual kubelet framework to get the defaultPod status
// we should query the actual runtime info and translate them in to V1PodStatus accordingly
// todo: 目前 defaultPod 的 status 状态是直接从 arklet 中查询和转换的，但是没有时间戳信息，所以无法提交对应的事件和开始时间。
//
//	当未来 arklet 提供了更多的信息时，我们可以相应的设置时间。
func (b *BaseProvider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	bizInfos, err := b.queryAllBiz(ctx)
	if err != nil {
		return nil, err
	}

	pod := b.runtimeInfoStore.GetPodByKey(namespace + "/" + name)
	podStatus := &corev1.PodStatus{}
	if pod == nil {
		log.G(ctx).Info("Get Pod Status Failed Because Pod Not Found In Local Runtime")
		return nil, nil
	}

	bizModels := b.modelUtils.GetBizModelsFromCoreV1Pod(pod)
	// bizIdentity
	bizRuntimeInfos := make(map[string]*ark.ArkBizInfo)
	for _, info := range bizInfos {
		bizRuntimeInfos[b.modelUtils.GetBizIdentityFromBizInfo(&info)] = &info
	}

	// bizName -> container status
	containerStatuses := make(map[string]*corev1.ContainerStatus)
	isAllContainerReady := true
	isSomeContainerFailed := false
	/**
	todo: if arklet return installed timestamp, we can submit corresponding event and start time accordingly
	      for now, we can just keep them empty
	if further info is provided, we can set the time accordingly
	failedTime would be the earliest time of the failed container
	successTime would be the latest time of the success container
	startTime would be the earliest time of the all container
	*/
	for _, bizModel := range bizModels {
		info := bizRuntimeInfos[b.modelUtils.GetBizIdentityFromBizModel(bizModel)]
		containerStatus := b.modelUtils.TranslateArkBizInfoToV1ContainerStatus(bizModel, info)
		containerStatuses[bizModel.BizName] = containerStatus

		if !containerStatus.Ready {
			isAllContainerReady = false
		}

		if containerStatus.State.Terminated != nil {
			isSomeContainerFailed = true
		}
	}

	podStatus.PodIP = b.localIP
	podStatus.PodIPs = []corev1.PodIP{{IP: b.localIP}}

	podStatus.ContainerStatuses = make([]corev1.ContainerStatus, 0)
	for _, status := range containerStatuses {
		podStatus.ContainerStatuses = append(podStatus.ContainerStatuses, *status)
	}

	podStatus.Phase = corev1.PodPending
	if isAllContainerReady {
		podStatus.Phase = corev1.PodRunning
		podStatus.Conditions = []corev1.PodCondition{
			{
				Type:   "module.koupleless.io/installed",
				Status: corev1.ConditionTrue,
			},
			{
				Type:   "module.koupleless.io/ready",
				Status: corev1.ConditionTrue,
			},
		}
	}

	if isSomeContainerFailed {
		podStatus.Phase = corev1.PodFailed
		podStatus.Conditions = []corev1.PodCondition{
			{
				Type:   "basement.koupleless.io/installed",
				Status: corev1.ConditionFalse,
			},
			{
				Type:   "basement.koupleless.io/ready",
				Status: corev1.ConditionFalse,
			},
		}
	}

	return podStatus, nil
}

func (b *BaseProvider) GetPods(_ context.Context) ([]*corev1.Pod, error) {
	return b.runtimeInfoStore.GetPods(), nil
}

func (b *BaseProvider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	// todo: implement this by using the port to get the logs
	return nil, nil
}

func (b *BaseProvider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	panic("koupleless java virtual base does not support run")
}

func (b *BaseProvider) AttachToContainer(ctx context.Context, namespace, podName, containerName string, attach api.AttachIO) error {
	panic("koupleless java virtual base does not support attach")
}

func (b *BaseProvider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	// TODO implement later, with node status and pod status summary
	pods, _ := b.GetPods(ctx)
	podsSummary := make([]statsv1alpha1.PodStats, len(pods))
	for index, pod := range pods {
		podsSummary[index] = b.modelUtils.TranslatePodToSummaryPodStats(pod)
	}
	return &statsv1alpha1.Summary{
		Node: statsv1alpha1.NodeStats{
			NodeName:         "",
			SystemContainers: nil,
			StartTime:        metav1.Time{},
			CPU:              nil,
			Memory:           nil,
			Network:          nil,
			Fs:               nil,
			Runtime:          nil,
			Rlimit:           nil,
		},
		Pods: podsSummary,
	}, nil
}

func (b *BaseProvider) GetMetricsResource(ctx context.Context) ([]*io_prometheus_client.MetricFamily, error) {
	// todo: implement me
	return make([]*io_prometheus_client.MetricFamily, 0), nil
}

func (b *BaseProvider) PortForward(ctx context.Context, namespace, pod string, port int32, stream io.ReadWriteCloser) error {
	panic("koupleless java virtual base does not support port forward")
}
