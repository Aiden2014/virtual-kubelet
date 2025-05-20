package provider

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/koupleless/virtual-kubelet/common/tracker"
	"github.com/koupleless/virtual-kubelet/model"
	"github.com/koupleless/virtual-kubelet/tunnel"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MockTracker is a mock implementation of the tracker interface
type MockTracker struct{}

func (m *MockTracker) Init() {}

func (m *MockTracker) FuncTrack(traceID string, scene string, event string, labels map[string]string, fn func() (error, model.ErrorCode)) error {
	err, _ := fn()
	return err
}

func (m *MockTracker) Eventually(traceID string, scene string, event string, labels map[string]string, code model.ErrorCode, fn func(context.Context) (bool, error), timeout time.Duration, interval time.Duration, successFn func(), failureFn func()) {
	// Do nothing in mock
}

func (m *MockTracker) ErrorReport(traceID string, scene string, event string, message string, labels map[string]string, code model.ErrorCode) {
	// Do nothing in mock
}

func init() {
	// Set the global tracker to our mock implementation
	tracker.SetTracker(&MockTracker{})
}

func TestSyncRelatedPodStatus(t *testing.T) {
	provider := NewVPodProvider("default", "127.0.0.1", "123", nil, nil, &tunnel.MockTunnel{})
	provider.syncBizStatusToKube(context.TODO(), model.BizStatusData{
		Key:        "test-biz-key",
		Name:       "test-name",
		PodKey:     "test-pod-key",
		State:      "test-state",
		ChangeTime: time.Now(),
		Reason:     "test-reason",
		Message:    "test-message",
	})
}

func TestSyncAllContainerInfo(t *testing.T) {
	tl := &tunnel.MockTunnel{}
	provider := NewVPodProvider("default", "127.0.0.1", "123", nil, &informertest.FakeInformers{}, tl)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: "test-image",
				},
			},
		},
	}
	provider.vPodStore.podKeyToPod = map[string]*corev1.Pod{
		"test": pod,
		"test2": {
			ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.Time{Time: time.Now().Add(time.Second)},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container-2",
						Image: "test-image",
					},
				},
			},
		},
	}
	provider.SyncAllBizStatusToKube(context.TODO(), []model.BizStatusData{
		{
			Key: tl.GetBizUniqueKey(&corev1.Container{
				Name: "test-container",
			}),
			PodKey: "namespace/name",
		},
	})
}

func TestUpdateDeletedPod(t *testing.T) {
	tl := &tunnel.MockTunnel{}
	provider := NewVPodProvider("default", "127.0.0.1", "123", nil, nil, tl)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.Time{Time: time.Now()},
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: "test-image",
				},
			},
		},
	}
	err := provider.UpdatePod(context.TODO(), pod)
	assert.NoError(t, err)
}

type mockTunnel struct {
	startBizCalled bool
	stopBizCalled  bool
	startBizError  error
	stopBizError   error
}

func (m *mockTunnel) StartBiz(nodeName, podKey string, container *corev1.Container) error {
	m.startBizCalled = true
	return m.startBizError
}

func (m *mockTunnel) StopBiz(nodeName, podKey string, container *corev1.Container) error {
	m.stopBizCalled = true
	return m.stopBizError
}

func (m *mockTunnel) FetchHealthData(nodeName string) error {
	return nil
}

func (m *mockTunnel) GetBizUniqueKey(container *corev1.Container) string {
	return container.Name
}

func (m *mockTunnel) Key() string {
	return "mock-tunnel"
}

func (m *mockTunnel) OnNodeNotReady(nodeName string) {
	// Do nothing in mock
}

func (m *mockTunnel) QueryAllBizStatusData(nodeName string) error {
	return nil
}

func (m *mockTunnel) Ready() bool {
	return true
}

func (m *mockTunnel) RegisterCallback(onBaseDiscovered tunnel.OnBaseDiscovered, onBaseStatusArrived tunnel.OnBaseStatusArrived, onAllBizStatusArrived tunnel.OnAllBizStatusArrived, onSingleBizStatusArrived tunnel.OnSingleBizStatusArrived) {
	// Do nothing in mock
}

func (m *mockTunnel) RegisterNode(nodeInfo model.NodeInfo) error {
	return nil
}

func (m *mockTunnel) Start(nodeName, podKey string) error {
	return nil
}

func (m *mockTunnel) UnRegisterNode(nodeName string) {
	// Do nothing in mock
}

type mockCache struct {
	cache.Cache
}

func (m *mockCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return nil
}

type mockClient struct {
	client.Client
}

func (m *mockClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return nil
}

func TestVPodProvider_RevisionBasedDeletion(t *testing.T) {
	// Create a mock cache
	mockCache := &mockCache{}

	// Create a mock client
	mockClient := &mockClient{}

	// Create a mock tunnel
	mockTunnel := &mockTunnel{}

	// Create the provider with mock dependencies
	provider := NewVPodProvider("test-namespace", "127.0.0.1", "test-node", mockClient, mockCache, mockTunnel)

	// Set up the notify function
	provider.NotifyPods(context.Background(), func(pod *corev1.Pod) {})

	// Test case 1: Basic module replacement with revision
	t.Run("Basic module replacement with revision", func(t *testing.T) {
		// Create initial pod with revision 100
		initialPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				Annotations: map[string]string{
					model.AnnotationKeyOfPodRevision: "100",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image:1.0",
						Env: []corev1.EnvVar{
							{Name: "BIZ_VERSION", Value: "1.0"},
						},
					},
				},
			},
		}

		// Create pod
		err := provider.CreatePod(context.Background(), initialPod)
		assert.NoError(t, err)

		// Verify initial revision
		bizKey := "test-container:1.0"
		initialRevision := provider.vPodStore.GetBizRevision(bizKey)
		assert.Equal(t, int64(100), initialRevision)

		// Create new pod with higher revision
		newPod := initialPod.DeepCopy()
		newPod.Annotations[model.AnnotationKeyOfPodRevision] = "200"
		newPod.Spec.Containers[0].Image = "test-image:2.0"

		// Update pod
		err = provider.UpdatePod(context.Background(), newPod) /**/
		assert.NoError(t, err)

		// Verify new revision
		newRevision := provider.vPodStore.GetBizRevision(bizKey)
		assert.Equal(t, int64(200), newRevision)

		// Reset mock
		mockTunnel.stopBizCalled = false

		// Try to delete with old revision
		oldPod := initialPod.DeepCopy()
		oldPod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		err = provider.DeletePod(context.Background(), oldPod)
		assert.NoError(t, err)

		// Verify that StopBiz was not called (due to lower revision)
		assert.False(t, mockTunnel.stopBizCalled, "StopBiz should not be called for lower revision")
	})

	// Test case 2: Multiple module replacements
	t.Run("Multiple module replacements", func(t *testing.T) {
		// Reset mock
		mockTunnel.stopBizCalled = false
		mockTunnel.startBizCalled = false

		// Create initial pod
		initialPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-2",
				Namespace: "default",
				Annotations: map[string]string{
					model.AnnotationKeyOfPodRevision: "100",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container-2",
						Image: "test-image:1.0",
					},
				},
			},
		}

		// Create pod
		err := provider.CreatePod(context.Background(), initialPod)
		assert.NoError(t, err)

		// Update to revision 200
		podV2 := initialPod.DeepCopy()
		podV2.Annotations[model.AnnotationKeyOfPodRevision] = "200"
		podV2.Spec.Containers[0].Image = "test-image:2.0"
		err = provider.UpdatePod(context.Background(), podV2)
		assert.NoError(t, err)

		// Update to revision 300
		podV3 := podV2.DeepCopy()
		podV3.Annotations[model.AnnotationKeyOfPodRevision] = "300"
		podV3.Spec.Containers[0].Image = "test-image:3.0"
		err = provider.UpdatePod(context.Background(), podV3)
		assert.NoError(t, err)

		// Reset mock
		mockTunnel.stopBizCalled = false

		// Try to delete with revision 100
		oldPod := initialPod.DeepCopy()
		oldPod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		err = provider.DeletePod(context.Background(), oldPod)
		assert.NoError(t, err)
		assert.False(t, mockTunnel.stopBizCalled, "StopBiz should not be called for old revision")

		// Try to delete with revision 200
		podV2.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		err = provider.DeletePod(context.Background(), podV2)
		assert.NoError(t, err)
		assert.False(t, mockTunnel.stopBizCalled, "StopBiz should not be called for intermediate revision")

		// Try to delete with revision 300
		podV3.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		err = provider.DeletePod(context.Background(), podV3)
		assert.NoError(t, err)
		assert.True(t, mockTunnel.stopBizCalled, "StopBiz should be called for current revision")
	})

	// Test case 3: Concurrent module updates
	t.Run("Concurrent module updates", func(t *testing.T) {
		// Reset mock
		mockTunnel.stopBizCalled = false
		mockTunnel.startBizCalled = false

		// Create initial pod
		initialPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-3",
				Namespace: "default",
				Annotations: map[string]string{
					model.AnnotationKeyOfPodRevision: "100",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container-3",
						Image: "test-image:1.0",
					},
				},
			},
		}

		// Create pod
		err := provider.CreatePod(context.Background(), initialPod)
		assert.NoError(t, err)

		// Simulate concurrent updates
		done := make(chan bool)
		for i := 0; i < 5; i++ {
			go func(index int) {
				pod := initialPod.DeepCopy()
				pod.Annotations[model.AnnotationKeyOfPodRevision] = strconv.Itoa(200 + index)
				pod.Spec.Containers[0].Image = "test-image:2.0"
				_ = provider.UpdatePod(context.Background(), pod)
				done <- true
			}(i)
		}

		// Wait for all updates to complete
		for i := 0; i < 5; i++ {
			<-done
		}

		// Reset mock
		mockTunnel.stopBizCalled = false

		// Try to delete with old revision
		initialPod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		err = provider.DeletePod(context.Background(), initialPod)
		assert.NoError(t, err)
		assert.False(t, mockTunnel.stopBizCalled, "StopBiz should not be called for old revision")
	})

	// Test case 4: Module status sync with revision
	t.Run("Module status sync with revision", func(t *testing.T) {
		// Reset mock
		mockTunnel.stopBizCalled = false
		mockTunnel.startBizCalled = false

		// Create pod with revision
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-4",
				Namespace: "default",
				Annotations: map[string]string{
					model.AnnotationKeyOfPodRevision: "100",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container-4",
						Image: "test-image:1.0",
						Env: []corev1.EnvVar{
							{Name: "BIZ_VERSION", Value: "1.0"},
						},
					},
				},
			},
		}

		// Create pod
		err := provider.CreatePod(context.Background(), pod)
		assert.NoError(t, err)

		// Sync status with higher revision
		bizStatus := model.BizStatusData{
			Key:        "test-container-4:1.0",
			Name:       "test-container-4",
			PodKey:     "default/test-pod-4",
			State:      "Running",
			ChangeTime: time.Now(),
			Revision:   200,
		}

		provider.SyncBizStatusToKube(context.Background(), bizStatus)

		// Verify revision was updated
		currentRevision := provider.vPodStore.GetBizRevision("test-container-4:1.0")
		assert.Equal(t, int64(200), currentRevision)

		// Reset mock
		mockTunnel.stopBizCalled = false

		// Try to delete with old revision
		pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		err = provider.DeletePod(context.Background(), pod)
		assert.NoError(t, err)
		assert.False(t, mockTunnel.stopBizCalled, "StopBiz should not be called for old revision")
	})
}
