package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/kubeshop/kubtest/pkg/api/kubtest"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type Client struct {
	ClientSet *kubernetes.Clientset
	Namespace string
	Cmd       string
}

func NewClient() (*Client, error) {
	clientSet, err := connectToK8s()
	if err != nil {
		return nil, err
	}

	return &Client{
		ClientSet: clientSet,
		Namespace: "default",
	}, nil
}

func (c *Client) LaunchK8sJob(jobName string, execution kubtest.Execution) *kubtest.ExecutionResult {
	jobs := c.ClientSet.BatchV1().Jobs(c.Namespace)
	var result string

	jsn, _ := json.Marshal(execution)
	image := "jasmingacic/test"

	if err := c.CreatePersistentVolume(jobName); err != nil {
		return &kubtest.ExecutionResult{
			Status:       kubtest.ExecutionStatusError,
			ErrorMessage: err.Error(),
		}
	}

	if err := wait.PollImmediate(time.Second, time.Duration(0)*time.Second, isPersistentVolumeBound(c.ClientSet, jobName, c.Namespace)); err != nil {
		return &kubtest.ExecutionResult{
			Status:       kubtest.ExecutionStatusError,
			ErrorMessage: err.Error(),
		}
	}

	if err := c.CreatePersistentVolumeClaim(jobName); err != nil {
		return &kubtest.ExecutionResult{
			Status:       kubtest.ExecutionStatusError,
			ErrorMessage: err.Error(),
		}
	}
	if err := wait.PollImmediate(time.Second, time.Duration(0)*time.Second, isPersistentVolumeClaimBound(c.ClientSet, jobName, c.Namespace)); err != nil {
		return &kubtest.ExecutionResult{
			Status:       kubtest.ExecutionStatusError,
			ErrorMessage: err.Error(),
		}
	}

	var TTLSecondsAfterFinished int32 = 1
	var backOffLimit int32 = 0
	jobSpec := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: c.Namespace,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &TTLSecondsAfterFinished,
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:            jobName,
							Image:           image,
							Command:         []string{"agent", string(jsn)},
							ImagePullPolicy: v1.PullAlways,
							VolumeMounts: []v1.VolumeMount{
								{
									MountPath: "/",
									Name:      jobName,
								},
							},
						},
					},
					RestartPolicy: v1.RestartPolicyNever,
					Volumes: []v1.Volume{
						{
							Name: jobName,
							VolumeSource: v1.VolumeSource{
								PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
									ClaimName: jobName,
								},
							},
						},
					},
				},
			},
			BackoffLimit: &backOffLimit,
		},
	}

	_, err := jobs.Create(context.TODO(), jobSpec, metav1.CreateOptions{})
	if err != nil {
		log.Println("Failed to create K8s job.", err)
	}

	//print job details
	// time.Sleep(2 * time.Second)

	pods, err := c.ClientSet.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	// pods, err := c.ClientSet.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return &kubtest.ExecutionResult{
			Status:       kubtest.ExecutionStatusError,
			ErrorMessage: err.Error(),
		}
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodSucceeded {
			if pod.Labels["job-name"] == jobName {
				if err := wait.PollImmediate(time.Second, time.Duration(0)*time.Second, isPodRunning(c.ClientSet, pod.Name, c.Namespace)); err != nil {
					return &kubtest.ExecutionResult{
						Status:       kubtest.ExecutionStatusError,
						ErrorMessage: err.Error(),
					}
				}
			}
			result, err = c.GetPodLogs(pod.Name, jobName, execution.Id)
			if err != nil {
				return &kubtest.ExecutionResult{
					Status:       kubtest.ExecutionStatusError,
					ErrorMessage: err.Error(),
				}
			}
		}
	}

	return &kubtest.ExecutionResult{
		Status:    kubtest.ExecutionStatusSuceess,
		RawOutput: result,
	}
}

// connectToK8s returns ClientSet
func connectToK8s() (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

// isPodRunning check if the pod in question is running state
func isPodRunning(c *kubernetes.Clientset, podName, namespace string) wait.ConditionFunc {
	return func() (bool, error) {
		pod, err := c.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		switch pod.Status.Phase {
		case v1.PodRunning, v1.PodSucceeded:
			return true, nil
		case v1.PodFailed:
			return false, nil
		}
		return false, nil
	}
}

func isPersistentVolumeBound(c *kubernetes.Clientset, podName, namespace string) wait.ConditionFunc {
	return func() (bool, error) {
		pv, err := c.CoreV1().PersistentVolumes().Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		switch pv.Status.Phase {
		case v1.VolumeBound, v1.VolumeAvailable:
			return true, nil
		case v1.VolumeFailed:
			return false, nil
		}
		return false, nil
	}
}

func isPersistentVolumeClaimBound(c *kubernetes.Clientset, podName, namespace string) wait.ConditionFunc {
	return func() (bool, error) {
		pv, err := c.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		switch pv.Status.Phase {
		case v1.ClaimBound:
			return true, nil
		case v1.ClaimLost:
			return false, nil
		}
		return false, nil
	}
}

func (c *Client) GetPodLogs(podName string, containerName string, endMessage string) (string, error) {
	count := int64(100)
	var toReturn string
	var message string
	podLogOptions := v1.PodLogOptions{
		Follow:    true,
		TailLines: &count,
	}

	podLogRequest := c.ClientSet.CoreV1().
		Pods(c.Namespace).
		GetLogs(podName, &podLogOptions)
	stream, err := podLogRequest.Stream(context.TODO())
	if err != nil {
		return "", err
	}

	defer stream.Close()

	for {
		buf := make([]byte, 2000)
		numBytes, err := stream.Read(buf)
		if numBytes == 0 {
			break
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		message = string(buf[:numBytes])
		if strings.Contains(message, fmt.Sprintf("$$$%s$$$", endMessage)) {
			message = ""
			break
		} else {
			toReturn += message
		}
	}
	return toReturn, nil
}

func (c *Client) CreatePersistentVolume(name string) error {
	quantity, err := resource.ParseQuantity("10Gi")
	if err != nil {
		return err
	}
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"type": "local"},
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity:    v1.ResourceList{"storage": quantity},
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: fmt.Sprintf("/mnt/data/%s", name),
				},
			},
			StorageClassName: "manual",
		},
	}

	if _, err = c.ClientSet.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{}); err != nil {
		return err
	}

	return nil
}

func (c *Client) CreatePersistentVolumeClaim(name string) error {
	storageClassName := "manual"
	quantity, err := resource.ParseQuantity("10Gi")
	if err != nil {
		return err
	}

	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClassName,
			AccessModes:      []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{"storage": quantity},
			},
		},
	}

	if _, err := c.ClientSet.CoreV1().PersistentVolumeClaims(c.Namespace).Create(context.TODO(), pvc, metav1.CreateOptions{}); err != nil {
		return err
	}
	return nil
}
