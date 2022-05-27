package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	zlog "github.com/rs/zerolog/log"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kubectl/pkg/cmd/cp"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	TempDebugManifest          = "tmp-manifest.yaml"
	LogPollInterval            = 2 * time.Second
	ContainerStatePollInterval = 2 * time.Second
)

// K8sClient high level k8s client
type K8sClient struct {
	ClientSet  *kubernetes.Clientset
	RESTConfig *rest.Config
}

// GetLocalK8sDeps get local k8s context config
func GetLocalK8sDeps() (*kubernetes.Clientset, *rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	k8sConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, err
	}
	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return nil, nil, err
	}
	return k8sClient, k8sConfig, nil
}

// NewK8sClient creates a new k8s client with a REST config
func NewK8sClient() *K8sClient {
	cs, cfg, err := GetLocalK8sDeps()
	if err != nil {
		zlog.Fatal().Err(err).Send()
	}
	return &K8sClient{
		ClientSet:  cs,
		RESTConfig: cfg,
	}
}

// ListPods lists pods for a namespace and selector
func (m *K8sClient) ListPods(namespace, selector string) (*v1.PodList, error) {
	return m.ClientSet.CoreV1().Pods(namespace).List(context.Background(), metaV1.ListOptions{LabelSelector: selector})
}

func (m *K8sClient) ListNamespaces(selector string) (*v1.NamespaceList, error) {
	return m.ClientSet.CoreV1().Namespaces().List(context.Background(), metaV1.ListOptions{LabelSelector: selector})
}

func (m *K8sClient) UniqueLabels(namespace string, selector string) ([]string, error) {
	uniqueLabels := make([]string, 0)
	isUnique := make(map[string]bool)
	k8sPods := m.ClientSet.CoreV1().Pods(namespace)
	podList, err := k8sPods.List(context.Background(), metaV1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}
	for _, p := range podList.Items {
		appLabel := p.Labels["app"]
		if _, ok := isUnique[appLabel]; !ok {
			uniqueLabels = append(uniqueLabels, appLabel)
		}
	}
	zlog.Info().
		Interface("AppLabels", uniqueLabels).
		Msg("Apps found")
	return uniqueLabels, nil
}

// Poll up to timeout seconds for pod to enter running state.
// Returns an error if the pod never enters the running state.
func waitForPodRunning(c kubernetes.Interface, namespace, podName string, timeout time.Duration) error {
	return wait.PollImmediate(2*time.Second, timeout, isPodRunning(c, podName, namespace))
}

// return a condition function that indicates whether the given pod is
// currently running
func isPodRunning(c kubernetes.Interface, podName, namespace string) wait.ConditionFunc {
	return func() (bool, error) {
		pod, err := c.CoreV1().Pods(namespace).Get(context.Background(), podName, metaV1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch pod.Status.Phase {
		case v1.PodRunning:
			return true, nil
		case v1.PodFailed:
			return false, errors.New("pod failed")
		case v1.PodSucceeded:
			return false, errors.New("pod succeeded, are we expecting a Job type")
		}
		return false, nil
	}
}

// ManifestOutput and interface to interact with a deployed environment
type ManifestOutput interface {
	SetNamespace(ns string)
	GetNamespace() string
	GetReadyCheckData() ReadyCheckData
	ProcessConnections(fwd *Forwarder) (map[string][]string, error)
}

func (m *K8sClient) AddLabel(namespace string, pod v1.Pod, key, value string) error {
	labelPatch := fmt.Sprintf(`[{"op":"add","path":"/metadata/labels/%s","value":"%s" }]`, key, value)
	_, err := m.ClientSet.CoreV1().Pods(namespace).Patch(context.Background(), pod.GetName(), types.JSONPatchType, []byte(labelPatch), metaV1.PatchOptions{})
	if err != nil {
		return err
	}
	return nil
}

// EnumerateInstances enumerate pods with instance label
func (m *K8sClient) EnumerateInstances(namespace string, selector string) error {
	k8sPods := m.ClientSet.CoreV1().Pods(namespace)
	podList, err := k8sPods.List(context.Background(), metaV1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return err
	}
	for id, pod := range podList.Items {
		if err := m.AddLabel(namespace, pod, "instance", strconv.Itoa(id)); err != nil {
			return err
		}
	}
	return nil
}

// WaitContainersReady waits until all containers ReadinessChecks are passed
func (m *K8sClient) WaitContainersReady(c ManifestOutput) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.GetReadyCheckData().Timeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return errors.New("timeout waiting container readiness probes")
		default:
			podList, err := m.ListPods(c.GetNamespace(), c.GetReadyCheckData().ReadinessProbeCheckSelector)
			if err != nil {
				return err
			}
			if len(podList.Items) == 0 {
				return fmt.Errorf("no pods in %s with selector %s", c.GetNamespace(), c.GetReadyCheckData().Timeout)
			}
			zlog.Info().Interface("Pods", podNames(podList)).Msg("Waiting for pods readiness probes")
			allReady := true
			for _, pod := range podList.Items {
				for _, c := range pod.Status.ContainerStatuses {
					if !c.Ready {
						zlog.Debug().
							Str("Pod", pod.Name).
							Str("Container", c.Name).
							Interface("Ready", c.Ready).
							Msg("Container readiness")
						allReady = false
					}
				}
			}
			if allReady {
				return nil
			}
			time.Sleep(ContainerStatePollInterval)
		}
	}
}

// WaitForPodBySelectorRunning Wait up to timeout seconds for all pods in 'namespace' with given 'selector' to enter running state.
// Returns an error if no pods are found or not all discovered pods enter running state.
func (m *K8sClient) WaitForPodBySelectorRunning(c ManifestOutput) error {
	podList, err := m.ListPods(c.GetNamespace(), c.GetReadyCheckData().ReadinessProbeCheckSelector)
	if err != nil {
		return err
	}
	if len(podList.Items) == 0 {
		return fmt.Errorf("no pods in %s with selector %s", c.GetNamespace(), c.GetReadyCheckData().Timeout)
	}

	zlog.Info().Interface("Pods", podNames(podList)).Msg("Waiting for pods in state Running")
	for _, pod := range podList.Items {
		if err := waitForPodRunning(m.ClientSet, c.GetNamespace(), pod.Name, c.GetReadyCheckData().Timeout); err != nil {
			return err
		}
	}
	return nil
}

// WaitLogMessages waits for log messages substrings
func (m *K8sClient) WaitLogMessages(c ManifestOutput) error {
	pods, err := m.ListPods(c.GetNamespace(), c.GetReadyCheckData().Selector)
	if err != nil {
		return err
	}

	zlog.Info().Interface("Pods", podNames(pods)).Str("Substring", c.GetReadyCheckData().LogSubStr).Msg("Searching for logs")
	logLinesFound := 0
	tail := int64(1000)
	ctx, cancel := context.WithTimeout(context.Background(), c.GetReadyCheckData().Timeout)
	defer cancel()
	// we can't stream and iterate, because container may crash, so send new request every time
	for {
		select {
		case <-ctx.Done():
			return errors.New("timeout waiting for logs")
		default:
			time.Sleep(LogPollInterval)
			for _, pod := range pods.Items {
				stream, err := m.ClientSet.CoreV1().
					Pods(c.GetNamespace()).
					GetLogs(pod.Name, &v1.PodLogOptions{
						Follow:    false,
						Container: c.GetReadyCheckData().Container,
						TailLines: &tail,
					}).Stream(ctx)
				if err != nil {
					return err
				}
				reader := bufio.NewScanner(stream)
				for reader.Scan() {
					select {
					case <-ctx.Done():
						return nil
					default:
						if strings.Contains(reader.Text(), c.GetReadyCheckData().LogSubStr) {
							logLinesFound++
						}
					}
				}
				if logLinesFound == len(pods.Items) {
					zlog.Info().Msg("All log substrings have been found")
					cancel()
					return nil
				}
			}
		}
	}
}

// NamespaceExists check if namespace exists
func (m *K8sClient) NamespaceExists(namespace string) bool {
	if _, err := m.ClientSet.CoreV1().Namespaces().Get(context.Background(), namespace, metaV1.GetOptions{}); err != nil {
		return false
	}
	return true
}

// RemoveNamespace removes namespace
func (m *K8sClient) RemoveNamespace(namespace string) error {
	zlog.Info().Str("Namespace", namespace).Msg("Removing namespace")
	if err := m.ClientSet.CoreV1().Namespaces().Delete(context.Background(), namespace, metaV1.DeleteOptions{}); err != nil {
		return err
	}
	return nil
}

type ReadyCheckData struct {
	ReadinessProbeCheckSelector string
	Selector                    string
	Container                   string
	LogSubStr                   string
	Timeout                     time.Duration
}

// CheckReady application heath check using ManifestOutputData params
func (m *K8sClient) CheckReady(c ManifestOutput) error {
	if err := m.WaitForPodBySelectorRunning(c); err != nil {
		return err
	}
	return m.WaitContainersReady(c)
}

func (m *K8sClient) Apply(manifest string) error {
	zlog.Info().Msg("Applying manifest")
	if err := os.WriteFile(TempDebugManifest, []byte(manifest), os.ModePerm); err != nil {
		return err
	}
	cmd := fmt.Sprintf("kubectl apply -f %s", TempDebugManifest)
	return ExecCmd(cmd)
}

func (m *K8sClient) Create(manifest string) error {
	zlog.Info().Msg("Creating manifest")
	if err := os.WriteFile(TempDebugManifest, []byte(manifest), os.ModePerm); err != nil {
		return err
	}
	cmd := fmt.Sprintf("kubectl create -f %s", TempDebugManifest)
	return ExecCmd(cmd)
}

func (m *K8sClient) DryRun(manifest string) error {
	zlog.Info().Msg("Creating manifest")
	if err := os.WriteFile(TempDebugManifest, []byte(manifest), os.ModePerm); err != nil {
		return err
	}
	return nil
}

// CopyToPod copies src to a particular container. Destination should be in the form of a proper K8s destination path
// NAMESPACE/POD_NAME:folder/FILE_NAME
func (m *K8sClient) CopyToPod(namespace, src, destination, containername string) (*bytes.Buffer, *bytes.Buffer, *bytes.Buffer, error) {
	m.RESTConfig.APIPath = "/api"
	m.RESTConfig.GroupVersion = &schema.GroupVersion{Version: "v1"} // this targets the core api groups so the url path will be /api/v1
	m.RESTConfig.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: scheme.Codecs}
	ioStreams, in, out, errOut := genericclioptions.NewTestIOStreams()

	copyOptions := cp.NewCopyOptions(ioStreams)
	copyOptions.Clientset = m.ClientSet
	copyOptions.ClientConfig = m.RESTConfig
	copyOptions.Container = containername
	copyOptions.Namespace = namespace

	formatted, err := regexp.MatchString(".*?\\/.*?\\:.*", destination)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Could not run copy operation: %v", err)
	}
	if !formatted {
		return nil, nil, nil, fmt.Errorf("destination string improperly formatted, see reference 'NAMESPACE/POD_NAME:folder/FILE_NAME'")
	}

	zlog.Debug().
		Str("Namespace", namespace).
		Str("Source", src).
		Str("Destination", destination).
		Str("Container", containername).
		Msg("Uploading file to pod")

	err = copyOptions.Run([]string{src, destination})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Could not run copy operation: %v", err)
	}
	return in, out, errOut, nil
}

func podNames(podItems *v1.PodList) []string {
	pn := make([]string, 0)
	for _, p := range podItems.Items {
		pn = append(pn, p.Name)
	}
	return pn
}
