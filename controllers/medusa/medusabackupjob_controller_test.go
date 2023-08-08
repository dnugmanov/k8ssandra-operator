package medusa

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	cassdcapi "github.com/k8ssandra/cass-operator/apis/cassandra/v1beta1"
	k8ss "github.com/k8ssandra/k8ssandra-operator/apis/k8ssandra/v1alpha1"
	api "github.com/k8ssandra/k8ssandra-operator/apis/medusa/v1alpha1"
	replicationapi "github.com/k8ssandra/k8ssandra-operator/apis/replication/v1alpha1"
	"github.com/k8ssandra/k8ssandra-operator/pkg/images"
	"github.com/k8ssandra/k8ssandra-operator/pkg/medusa"
	"github.com/k8ssandra/k8ssandra-operator/pkg/shared"
	"github.com/k8ssandra/k8ssandra-operator/pkg/utils"
	"github.com/k8ssandra/k8ssandra-operator/test/framework"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	medusaImageRepo     = "test/medusa"
	cassandraUserSecret = "medusa-secret"
	defaultBackupName   = "backup1"
	dc1PodPrefix        = "192.168.1."
	dc2PodPrefix        = "192.168.2."
)

func testMedusaBackupDatacenter(t *testing.T, ctx context.Context, f *framework.Framework, namespace string) {
	require := require.New(t)

	k8sCtx0 := f.DataPlaneContexts[0]

	kc := &k8ss.K8ssandraCluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "test",
		},
		Spec: k8ss.K8ssandraClusterSpec{
			Cassandra: &k8ss.CassandraClusterTemplate{
				Datacenters: []k8ss.CassandraDatacenterTemplate{
					{
						Meta: k8ss.EmbeddedObjectMeta{
							Name: "dc1",
						},
						K8sContext: k8sCtx0,
						Size:       3,
						DatacenterOptions: k8ss.DatacenterOptions{
							ServerVersion: "3.11.14",
							StorageConfig: &cassdcapi.StorageConfig{
								CassandraDataVolumeClaimSpec: &corev1.PersistentVolumeClaimSpec{
									StorageClassName: &defaultStorageClass,
								},
							},
						},
					},
				},
			},
			Medusa: &api.MedusaClusterTemplate{
				ContainerImage: &images.Image{
					Repository: medusaImageRepo,
				},
				StorageProperties: api.Storage{
					StorageSecretRef: corev1.LocalObjectReference{
						Name: cassandraUserSecret,
					},
				},
				CassandraUserSecretRef: corev1.LocalObjectReference{
					Name: cassandraUserSecret,
				},
			},
		},
	}

	t.Log("Creating k8ssandracluster with Medusa")
	err := f.Client.Create(ctx, kc)
	require.NoError(err, "failed to create K8ssandraCluster")

	reconcileReplicatedSecret(ctx, t, f, kc)
	reconcileMedusaStandaloneDeployment(ctx, t, f, kc, "dc1", f.DataPlaneContexts[0])
	t.Log("check that dc1 was created")
	dc1Key := framework.NewClusterKey(f.DataPlaneContexts[0], namespace, "dc1")
	require.Eventually(f.DatacenterExists(ctx, dc1Key), timeout, interval)

	t.Log("update datacenter status to scaling up")
	err = f.PatchDatacenterStatus(ctx, dc1Key, func(dc *cassdcapi.CassandraDatacenter) {
		dc.SetCondition(cassdcapi.DatacenterCondition{
			Type:               cassdcapi.DatacenterScalingUp,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
		})
	})
	require.NoError(err, "failed to patch datacenter status")

	kcKey := framework.NewClusterKey(f.ControlPlaneContext, namespace, "test")

	t.Log("check that the K8ssandraCluster status is updated")
	require.Eventually(func() bool {
		kc := &k8ss.K8ssandraCluster{}
		err = f.Get(ctx, kcKey, kc)

		if err != nil {
			t.Logf("failed to get K8ssandraCluster: %v", err)
			return false
		}

		if len(kc.Status.Datacenters) == 0 {
			return false
		}

		k8ssandraStatus, found := kc.Status.Datacenters[dc1Key.Name]
		if !found {
			t.Logf("status for datacenter %s not found", dc1Key)
			return false
		}

		condition := findDatacenterCondition(k8ssandraStatus.Cassandra, cassdcapi.DatacenterScalingUp)
		return condition != nil && condition.Status == corev1.ConditionTrue
	}, timeout, interval, "timed out waiting for K8ssandraCluster status update")

	dc1 := &cassdcapi.CassandraDatacenter{}
	err = f.Get(ctx, dc1Key, dc1)

	t.Log("update dc1 status to ready")
	err = f.PatchDatacenterStatus(ctx, dc1Key, func(dc *cassdcapi.CassandraDatacenter) {
		dc.Status.CassandraOperatorProgress = cassdcapi.ProgressReady
		dc.SetCondition(cassdcapi.DatacenterCondition{
			Type:               cassdcapi.DatacenterReady,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
		})
	})
	require.NoError(err, "failed to update dc1 status to ready")

	backupCreated := createAndVerifyMedusaBackup(dc1Key, dc1, f, ctx, require, t, namespace, defaultBackupName)
	require.True(backupCreated, "failed to create backup")

	t.Log("verify that medusa gRPC clients are invoked")
	require.Equal(map[string][]string{
		fmt.Sprintf("%s:%d", getPodIpAddress(0, dc1.DatacenterName()), shared.BackupSidecarPort): {defaultBackupName},
		fmt.Sprintf("%s:%d", getPodIpAddress(1, dc1.DatacenterName()), shared.BackupSidecarPort): {defaultBackupName},
		fmt.Sprintf("%s:%d", getPodIpAddress(2, dc1.DatacenterName()), shared.BackupSidecarPort): {defaultBackupName},
	}, medusaClientFactory.GetRequestedBackups(dc1.DatacenterName()))

	err = f.DeleteK8ssandraCluster(ctx, client.ObjectKey{Namespace: kc.Namespace, Name: kc.Name}, timeout, interval)
	require.NoError(err, "failed to delete K8ssandraCluster")
	verifyObjectDoesNotExist(ctx, t, f, dc1Key, &cassdcapi.CassandraDatacenter{})
}

func createAndVerifyMedusaBackup(dcKey framework.ClusterKey, dc *cassdcapi.CassandraDatacenter, f *framework.Framework, ctx context.Context, require *require.Assertions, t *testing.T, namespace, backupName string) bool {
	dcServiceKey := framework.NewClusterKey(dcKey.K8sContext, dcKey.Namespace, dc.GetAllPodsServiceName())
	dcService := &corev1.Service{}
	if err := f.Get(ctx, dcServiceKey, dcService); err != nil {
		if errors.IsNotFound(err) {
			dcService = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: dcServiceKey.Namespace,
					Name:      dcServiceKey.Name,
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						cassdcapi.ClusterLabel: cassdcapi.CleanLabelValue(dc.Spec.ClusterName),
					},
					Ports: []corev1.ServicePort{
						{
							Name: "cql",
							Port: 9042,
						},
					},
				},
			}

			err := f.Create(ctx, dcServiceKey, dcService)
			require.NoError(err)
		} else {
			t.Errorf("failed to get service %s: %v", dcServiceKey, err)
		}
	}

	createDatacenterPods(t, f, ctx, dcKey, dc)

	dcCopy := dc.DeepCopy()
	dcKeyCopy := framework.NewClusterKey(f.DataPlaneContexts[0], dcKey.Namespace+"-copy", dcKey.Name)
	dcCopy.ObjectMeta.Namespace = dc.Namespace + "-copy"

	createDatacenterPods(t, f, ctx, dcKeyCopy, dcCopy)

	t.Log("creating MedusaBackupJob")
	backupKey := framework.NewClusterKey(dcKey.K8sContext, dcKey.Namespace, backupName)
	backup := &api.MedusaBackupJob{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      backupName,
		},
		Spec: api.MedusaBackupJobSpec{
			CassandraDatacenter: dc.Name,
		},
	}

	err := f.Create(ctx, backupKey, backup)
	require.NoError(err, "failed to create MedusaBackupJob")

	t.Log("verify that the backups are started")
	require.Eventually(func() bool {
		t.Logf("Requested backups: %v", medusaClientFactory.GetRequestedBackups(dc.DatacenterName()))
		updated := &api.MedusaBackupJob{}
		err := f.Get(ctx, backupKey, updated)
		if err != nil {
			t.Logf("failed to get MedusaBackupJob: %v", err)
			return false
		}
		return !updated.Status.StartTime.IsZero()
	}, timeout, interval)

	t.Log("verify the backup finished")
	require.Eventually(func() bool {
		t.Logf("Requested backups: %v", medusaClientFactory.GetRequestedBackups(dc.DatacenterName()))
		updated := &api.MedusaBackupJob{}
		err := f.Get(ctx, backupKey, updated)
		if err != nil {
			return false
		}
		t.Logf("backup finish time: %v", updated.Status.FinishTime)
		t.Logf("backup finished: %v", updated.Status.Finished)
		t.Logf("backup in progress: %v", updated.Status.InProgress)
		return !updated.Status.FinishTime.IsZero() && len(updated.Status.Finished) == 3 && len(updated.Status.InProgress) == 0
	}, timeout, interval)

	require.Equal(int(dc.Spec.Size), len(medusaClientFactory.GetRequestedBackups(dc.DatacenterName())))

	return true
}

func reconcileReplicatedSecret(ctx context.Context, t *testing.T, f *framework.Framework, kc *k8ss.K8ssandraCluster) {
	t.Log("check ReplicatedSecret reconciled")

	rsec := &replicationapi.ReplicatedSecret{}
	replSecretKey := types.NamespacedName{Name: kc.Name, Namespace: kc.Namespace}

	assert.Eventually(t, func() bool {
		err := f.Client.Get(ctx, replSecretKey, rsec)
		return err == nil
	}, timeout, interval, "failed to get ReplicatedSecret")

	conditions := make([]replicationapi.ReplicationCondition, 0)
	now := metav1.Now()

	for _, target := range rsec.Spec.ReplicationTargets {
		conditions = append(conditions, replicationapi.ReplicationCondition{
			Cluster:            target.K8sContextName,
			Type:               replicationapi.ReplicationDone,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: &now,
		})
	}
	rsec.Status.Conditions = conditions
	err := f.Client.Status().Update(ctx, rsec)

	require.NoError(t, err, "Failed to update ReplicationSecret status")
}

// Creates a fake ip address with the pod's original index from the StatefulSet
func getPodIpAddress(index int, dcName string) string {
	switch dcName {
	case "dc1":
		return dc1PodPrefix + strconv.Itoa(50+index)
	case "dc2":
		return dc2PodPrefix + strconv.Itoa(50+index)
	default:
		return "192.168.3." + strconv.Itoa(50+index)
	}
}

type fakeMedusaClientFactory struct {
	clientsMutex sync.Mutex
	clients      map[string]*fakeMedusaClient
}

func NewMedusaClientFactory() *fakeMedusaClientFactory {
	return &fakeMedusaClientFactory{clients: make(map[string]*fakeMedusaClient, 0)}
}

func (f *fakeMedusaClientFactory) NewClient(address string) (medusa.Client, error) {
	f.clientsMutex.Lock()
	defer f.clientsMutex.Unlock()
	_, ok := f.clients[address]
	if !ok {
		if strings.HasPrefix(address, dc1PodPrefix) {
			f.clients[address] = newFakeMedusaClient("dc1")
		} else if strings.HasPrefix(address, dc2PodPrefix) {
			f.clients[address] = newFakeMedusaClient("dc2")
		} else {
			f.clients[address] = newFakeMedusaClient("")
		}
	}
	return f.clients[address], nil
}

func (f *fakeMedusaClientFactory) GetRequestedBackups(dc string) map[string][]string {
	f.clientsMutex.Lock()
	defer f.clientsMutex.Unlock()
	requestedBackups := make(map[string][]string)
	for k, v := range f.clients {
		if v.DcName == dc {
			requestedBackups[k] = v.RequestedBackups
		}
	}
	return requestedBackups
}

type fakeMedusaClient struct {
	RequestedBackups []string
	DcName           string
}

func newFakeMedusaClient(dcName string) *fakeMedusaClient {
	return &fakeMedusaClient{RequestedBackups: make([]string, 0), DcName: dcName}
}

func (c *fakeMedusaClient) Close() error {
	return nil
}

func (c *fakeMedusaClient) CreateBackup(ctx context.Context, name string, backupType string) error {
	c.RequestedBackups = append(c.RequestedBackups, name)
	return nil
}

func (c *fakeMedusaClient) GetBackups(ctx context.Context) ([]*medusa.BackupSummary, error) {
	backups := make([]*medusa.BackupSummary, 0)
	for _, name := range c.RequestedBackups {
		backup := &medusa.BackupSummary{
			BackupName: name,
			StartTime:  0,
			FinishTime: 10,
			Status:     *medusa.StatusType_SUCCESS.Enum(),
		}
		backups = append(backups, backup)
	}
	return backups, nil
}

func (c *fakeMedusaClient) BackupStatus(ctx context.Context, name string) (*medusa.BackupStatusResponse, error) {
	return nil, nil
}

func (c *fakeMedusaClient) PurgeBackups(ctx context.Context) (*medusa.PurgeBackupsResponse, error) {
	response := &medusa.PurgeBackupsResponse{
		NbBackupsPurged:           2,
		NbObjectsPurged:           10,
		TotalObjectsWithinGcGrace: 0,
		TotalPurgedSize:           1000,
	}
	return response, nil
}

func (c *fakeMedusaClient) PrepareRestore(ctx context.Context, datacenter, backupName, restoreKey string) (*medusa.PrepareRestoreResponse, error) {
	return nil, nil
}

func findDatacenterCondition(status *cassdcapi.CassandraDatacenterStatus, condType cassdcapi.DatacenterConditionType) *cassdcapi.DatacenterCondition {
	for _, condition := range status.Conditions {
		if condition.Type == condType {
			return &condition
		}
	}
	return nil
}

func createDatacenterPods(t *testing.T, f *framework.Framework, ctx context.Context, dcKey framework.ClusterKey, dc *cassdcapi.CassandraDatacenter) {
	_ = f.CreateNamespace(dcKey.Namespace)
	for i := int32(0); i < dc.Spec.Size; i++ {
		pod := &corev1.Pod{}
		podName := fmt.Sprintf("%s-%s-%d", dc.Spec.ClusterName, dc.DatacenterName(), i)
		podKey := framework.NewClusterKey(dcKey.K8sContext, dcKey.Namespace, podName)
		err := f.Get(ctx, podKey, pod)
		if err != nil {
			if errors.IsNotFound(err) {
				t.Logf("pod %s-%s-%d not found", dc.Spec.ClusterName, dc.DatacenterName(), i)
				pod = &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: dc.Namespace,
						Name:      podName,
						Labels: map[string]string{
							cassdcapi.ClusterLabel:    cassdcapi.CleanLabelValue(dc.Spec.ClusterName),
							cassdcapi.DatacenterLabel: dc.Name,
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "cassandra",
								Image: "cassandra",
							},
							{
								Name:  shared.BackupSidecarName,
								Image: shared.BackupSidecarName,
							},
						},
					},
				}
				err = f.Create(ctx, podKey, pod)
				require.NoError(t, err, "failed to create datacenter pod")

				patch := client.MergeFrom(pod.DeepCopy())
				pod.Status.PodIP = getPodIpAddress(int(i), dc.DatacenterName())

				err = f.PatchStatus(ctx, pod, patch, podKey)
				require.NoError(t, err, "failed to patch datacenter pod status")
			}
		}
	}
}

func verifyObjectDoesNotExist(ctx context.Context, t *testing.T, f *framework.Framework, key framework.ClusterKey, obj client.Object) {
	assert.Eventually(t, func() bool {
		err := f.Get(ctx, key, obj)
		return err != nil && errors.IsNotFound(err)
	}, timeout, interval, "failed to verify object does not exist", key)
}

func reconcileMedusaStandaloneDeployment(ctx context.Context, t *testing.T, f *framework.Framework, kc *k8ss.K8ssandraCluster, dcName string, k8sContext string) {
	t.Logf("start reconcileMedusaStandaloneDeployment for dc %s", dcName)

	medusaDepl := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      medusa.MedusaStandaloneDeploymentName(kc.SanitizedName(), dcName),
			Namespace: kc.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": medusa.MedusaStandaloneDeploymentName(kc.SanitizedName(), dcName)},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": medusa.MedusaStandaloneDeploymentName(kc.SanitizedName(), dcName)},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  medusa.MedusaStandaloneDeploymentName(kc.SanitizedName(), dcName),
							Image: "quay.io/k8ssandra/medusa:0.11.0",
						},
					},
				},
			},
		},
	}
	medusaKey := framework.ClusterKey{NamespacedName: utils.GetKey(medusaDepl), K8sContext: k8sContext}
	f.Create(ctx, medusaKey, medusaDepl)

	actualMedusaDepl := &appsv1.Deployment{}
	assert.Eventually(t, func() bool {
		err := f.Get(ctx, medusaKey, actualMedusaDepl)
		return err == nil
	}, timeout, interval, "failed to get Medusa Deployment")

	err := f.SetMedusaDeplAvailable(ctx, medusaKey)

	require.NoError(t, err, "Failed to update Medusa Deployment status")
}
