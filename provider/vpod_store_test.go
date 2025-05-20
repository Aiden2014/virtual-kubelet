package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestVPodStore_PutPod(t *testing.T) {
	store := NewVPodStore()
	store.PutPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "ns1",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "container1",
					Image: "image1",
				},
			},
		},
	})
	assert.NotNil(t, store.podKeyToPod["ns1/pod1"])
}

func TestVPodStore_DeletePod(t *testing.T) {
	store := NewVPodStore()
	store.PutPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "ns1",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "container1",
					Image: "image1",
				},
			},
		},
	})
	store.DeletePod("ns1/pod1")
	assert.Nil(t, store.podKeyToPod["ns1/pod1"])
}

func TestVPodStore_GetPodByKey(t *testing.T) {
	store := NewVPodStore()
	store.PutPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "ns1",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "container1",
					Image: "image1",
				},
			},
		},
	})
	p := store.GetPodByKey("ns1/pod1")
	assert.NotNil(t, p)
}

func TestVPodStore_GetPods(t *testing.T) {
	store := NewVPodStore()
	store.PutPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "ns1",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "container1",
					Image: "image1",
				},
			},
		},
	})
	ps := store.GetPods()
	assert.Len(t, ps, 1)
}

func TestVPodStore_Revision(t *testing.T) {
	store := NewVPodStore()

	// Test case 1: Basic revision update and get
	t.Run("Basic revision update and get", func(t *testing.T) {
		bizKey := "test-biz-1"
		revision := int64(100)

		// Update revision
		store.UpdateBizRevision(bizKey, revision)

		// Get revision
		gotRevision := store.GetBizRevision(bizKey)
		assert.Equal(t, revision, gotRevision, "Revision should match the updated value")
	})

	// Test case 2: ShouldDeleteBiz with non-existent biz
	t.Run("ShouldDeleteBiz with non-existent biz", func(t *testing.T) {
		bizKey := "non-existent-biz"
		revision := int64(100)

		// Should allow deletion for non-existent biz
		shouldDelete := store.ShouldDeleteBiz(bizKey, revision)
		assert.True(t, shouldDelete, "Should allow deletion for non-existent biz")
	})

	// Test case 3: ShouldDeleteBiz with lower revision
	t.Run("ShouldDeleteBiz with lower revision", func(t *testing.T) {
		bizKey := "test-biz-2"
		currentRevision := int64(200)
		lowerRevision := int64(100)

		// Update with current revision
		store.UpdateBizRevision(bizKey, currentRevision)

		// Try to delete with lower revision
		shouldDelete := store.ShouldDeleteBiz(bizKey, lowerRevision)
		assert.False(t, shouldDelete, "Should not allow deletion with lower revision")
	})

	// Test case 4: ShouldDeleteBiz with higher revision
	t.Run("ShouldDeleteBiz with higher revision", func(t *testing.T) {
		bizKey := "test-biz-3"
		currentRevision := int64(100)
		higherRevision := int64(200)

		// Update with current revision
		store.UpdateBizRevision(bizKey, currentRevision)

		// Try to delete with higher revision
		shouldDelete := store.ShouldDeleteBiz(bizKey, higherRevision)
		assert.True(t, shouldDelete, "Should allow deletion with higher revision")
	})

	// Test case 5: ShouldDeleteBiz with equal revision
	t.Run("ShouldDeleteBiz with equal revision", func(t *testing.T) {
		bizKey := "test-biz-4"
		revision := int64(100)

		// Update with revision
		store.UpdateBizRevision(bizKey, revision)

		// Try to delete with equal revision
		shouldDelete := store.ShouldDeleteBiz(bizKey, revision)
		assert.True(t, shouldDelete, "Should allow deletion with equal revision")
	})

	// Test case 6: Concurrent revision updates
	t.Run("Concurrent revision updates", func(t *testing.T) {
		bizKey := "test-biz-5"
		initialRevision := int64(100)
		finalRevision := int64(200)

		// Update with initial revision
		store.UpdateBizRevision(bizKey, initialRevision)

		// Simulate concurrent updates
		done := make(chan bool)
		for i := 0; i < 10; i++ {
			go func() {
				store.UpdateBizRevision(bizKey, finalRevision)
				done <- true
			}()
		}

		// Wait for all updates to complete
		for i := 0; i < 10; i++ {
			<-done
		}

		// Verify final revision
		gotRevision := store.GetBizRevision(bizKey)
		assert.Equal(t, finalRevision, gotRevision, "Final revision should be the highest value")
	})

	// Test case 7: Revision with pod operations
	t.Run("Revision with pod operations", func(t *testing.T) {
		bizKey := "test-biz-6"
		revision := int64(100)

		// Create a test pod
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				Annotations: map[string]string{
					"biz-key": bizKey,
				},
			},
		}

		// Update revision
		store.UpdateBizRevision(bizKey, revision)

		// Store pod
		store.PutPod(pod)

		// Verify pod exists
		retrievedPod := store.GetPodByKey("default/test-pod")
		assert.NotNil(t, retrievedPod, "Pod should exist in store")

		// Delete pod
		store.DeletePod("default/test-pod")

		// Verify pod is deleted
		retrievedPod = store.GetPodByKey("default/test-pod")
		assert.Nil(t, retrievedPod, "Pod should be deleted from store")

		// Verify revision still exists
		gotRevision := store.GetBizRevision(bizKey)
		assert.Equal(t, revision, gotRevision, "Revision should persist after pod deletion")
	})
}
