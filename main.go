package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
	corev1 "k8s.io/api/core/v1"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"golang.org/x/net/context"
	"gopkg.in/yaml.v2"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/api/core/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

var (
	kubeconfigFlag *string
	contextFlag    string
	nodeFlag       string
	nodeReference  *v1.Node
	modeFlag       string
	config         = Config{Timing: Timing{CheckInterval: 90, StopTimeout: 30} }
	kubeClient	   *kubernetes.Clientset
	kubeRecorder   record.EventRecorder
	wg             sync.WaitGroup
)

type Timing struct {

	CheckInterval uint32 `yaml:"check_interval"`

	StopTimeout uint32 `yaml:"stop_timeout"`

}

type Whitelist struct {

	Images     []string `yaml:"images"`

}

type Config struct {

	Timing Timing `yaml:"timing"`

	Whitelist Whitelist `yaml:"whitelist"`

}

func init() {

	if home := os.Getenv("HOME"); home != "" {
		kubeconfigFlag = flag.String("kubeconfig",
			filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfigFlag = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	log.Println("kubeconfig:", kubeconfigFlag)

	if node := os.Getenv("NODE"); node != "" {
		flag.StringVar(&nodeFlag, "node", os.Getenv("NODE"), "current node")
	} else {
		flag.StringVar(&nodeFlag, "node", "", "current node")
	}

	log.Println("node:", nodeFlag)

	if mode := os.Getenv("MODE"); mode != "" {
		flag.StringVar(&modeFlag, "mode", os.Getenv("MODE"), "current mode (remove or watch [default])")
	} else {
		flag.StringVar(&modeFlag, "mode", "watch", "current node")
	}

	log.Println("mode:", modeFlag)

	flag.StringVar(&contextFlag, "context", "", "context")

	log.Println("context:", contextFlag)

	flag.Parse()

	loadConfiguration()

	kubeClient = createK8sClient()

	kubeRecorder = getEventRecorder(kubeClient, nodeFlag, "container-checker")

	nodeReference = getNodeReference().DeepCopy()

	log.Println("config:", config)

}

// loadConfiguration reads the configuration YAML file.
func loadConfiguration() {

	fileData, err := ioutil.ReadFile("/config/config.yaml")

	if err != nil {
		log.Fatalln(err.Error())
		panic(err.Error())
	}

	yaml.Unmarshal(fileData, &config)

}

// compareContainerGroups compares the Docker containers list with the Kubernetes containers list. If an orphan,
// in the dockerGroup, is found; then it is returned to caller.
func compareContainerGroups(dockerGroup []types.Container, kubernetesGroup []string) []types.Container {

	var orphansFound []types.Container

	for _, container := range dockerGroup {

		if !stringInSlice(container.ID, &kubernetesGroup) {
			log.Println("Orphan Container Found:", container.ID, "(", container.Image, ") Age:", time.Since(time.Unix(container.Created, 0)))
			orphansFound = append(orphansFound, container)
		}

	}

	return orphansFound

}

// createK8sClient connects the application to a Kubernetes cluster.
func createK8sClient() *kubernetes.Clientset {

	config, err := rest.InClusterConfig()

	if err != nil {
		var configOverrides = clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: ""}, Context: clientcmdapi.Context{Cluster: contextFlag}}

		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfigFlag},
			&configOverrides).ClientConfig()

		if err != nil {
			log.Fatalln(err.Error())
			panic(err.Error())
		}
	}

	clientset, err := kubernetes.NewForConfig(config)

	if err != nil {
		log.Println("Error with connecting to cluster:", err.Error())
		panic(err.Error())
	}

	return clientset
}

// executeCheck performs the core functionality of this application: Look for outstanding docker containers that the
// Kubernetes API no longer knows about.
func executeCheck() {

	var dockerChannel = make(chan []types.Container)
	var k8sChannel = make(chan []string)
	var k8sLogMessageChannel = make(chan string)
	var kubernetesContainers []string
	var dockerContainers []types.Container

	wg.Add(2)

	go func() {
		kubernetesContainers = <-k8sChannel
		wg.Done()
	}()

	go func() {
		dockerContainers = <-dockerChannel
		wg.Done()
	}()

	go func() {
		for {
			msg := <-k8sLogMessageChannel
			sendEvent("DanglingContainer", msg)
		}
	}()

	go getPodContainers(k8sChannel)
	go getDockerContainers(dockerChannel)
	wg.Wait()

	orphans := compareContainerGroups(dockerContainers, kubernetesContainers)

	if len(orphans) > 0 {
		removeOrReportOrphanContainers(orphans, k8sLogMessageChannel)
	} else {
		log.Println("No orphaned containers found.")
	}
}

// getDockerContainers connects to the local Docker daemon to retrieve a list of running containers and removes
// the containers based on the criteria of the whitelist in Config.
func getDockerContainers(listChannel chan []types.Container) () {
	cli, err := docker.NewEnvClient()

	if err != nil {
		log.Println("Cannot connect to Docker daemon.", err.Error())
	}

	containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{})

	if err != nil {
		log.Println("No running containers found in Docker.", err.Error())
	}

	var filtered []types.Container

	for _, c := range containers {
		remove := false

		for _, image := range config.Whitelist.Images {
			if strings.Contains(c.Image, image) {
				remove = true
			}
		}

		if !remove {
			filtered = append(filtered, c)
		}

	}

	listChannel <- filtered
}

// getPodContainers connects to the Kubernetes API to retrieve a list of containers running in all pods running on
// the same node as the Docker daemon.
func getPodContainers(k8sChannel chan []string) {

	podList, err := kubeClient.CoreV1().Pods(metav1.NamespaceAll).List(metav1.ListOptions{})

	if err != nil {
		log.Println("Error in listing pods:", err.Error())
	}

	var containerIDs []string

	for _, pod := range podList.Items {

		if pod.Spec.NodeName == nodeFlag {

			for _, status := range pod.Status.ContainerStatuses {
				containerID := strings.TrimPrefix(status.ContainerID, "docker://")
				containerIDs = append(containerIDs, containerID)
			}

		}

	}

	k8sChannel <- containerIDs

}

// sendEvent places an event on the recorder.
func sendEvent(reason, messageFmt string) {
	kubeRecorder.Event(nodeReference, corev1.EventTypeWarning, reason, messageFmt)
}

// getEventRecorder generates a recorder for specific node name and source.
func getEventRecorder(c *kubernetes.Clientset, nodeName, source string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: source, Host: nodeName})
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: c.CoreV1().Events("")})
	return recorder
}

func getNodeReference() *v1.Node {
	node, err := kubeClient.CoreV1().Nodes().Get(nodeFlag, metav1.GetOptions{})
	if err != nil {
		log.Println("Node information was not retrieved:", err.Error())
		panic(err)
	}
	return node
}

// removeOrphanContainers iterates through the orphan containers and calls Docker ContainerStop on each container with
// 30 seconds timeout.
func removeOrReportOrphanContainers(orphans []types.Container, logChannel chan string) {


		cli, err := docker.NewEnvClient()

		if err != nil {
			log.Println("Cannot connect to Docker daemon.", err.Error())
		}

		var stopTimeout= time.Duration(config.Timing.StopTimeout) * time.Second


	for _, c := range orphans {

		if modeFlag == "remove" {

			log.Println("Stopping container:", c)
			cli.ContainerStop(context.Background(), c.ID, &stopTimeout)
			logChannel <- fmt.Sprintf("Dangling container stopped: %s (%s)", c.ID, c.ImageID)

		} else {

			log.Println("Observing dangling container:", c)
			logChannel <- fmt.Sprintf("Dangling container found: %s (%s)", c.ID, c.ImageID)

		}

	}

}

// stringInSlice return true if list contains the string.
func stringInSlice(a string, list *[]string) bool {

	for _, b := range *list {

		if strings.Contains(b, a) {
			return true
		}

	}

	return false
}

func main() {

	for {
		executeCheck()
		time.Sleep(time.Duration(config.Timing.CheckInterval) * time.Second)
	}

}
