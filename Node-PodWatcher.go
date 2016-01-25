package main

/* The goal of this testing is to determine the creation time of pods, services, and routes at different intervals. The testing will ecompass the following tasks:
1) Find the creation time of x pods per replication controller on an idle system
2) Find the creation time of x pods per replication controller on a system that is loaded with pod creation requests (100-200 pods)

The above two tasks should be perfomed on several different instance types.
*/

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/etcd/Godeps/_workspace/src/golang.org/x/net/context"
	etcd "github.com/coreos/etcd/client"
)

const (
	imageStreamsUrl      = "https://raw.githubusercontent.com/openshift/openshift-ansible/master/roles/openshift_examples/files/examples/v1.1/image-streams/image-streams-rhel7.json"
	jbossImageStreamsUrl = "https://raw.githubusercontent.com/openshift/openshift-ansible/master/roles/openshift_examples/files/examples/v1.1/xpaas-streams/jboss-image-streams.json"
)

var (
	host               string
	port               int
	tmpDir             string
	appTemplate        string
	adminKubeConfig    string
	registryKubeConfig string
	routerKubeConfig   string
	numUsers           int
	numAppsPerUser     int
	numReplicas        int
	numThreads         int
	verbose            bool
	cleanup            bool

	server        string
	caCrt         string
	etcdClientCrt string
	etcdClientKey string
	templateFile  string

	wg        sync.WaitGroup
	tests     []Test
	startTime time.Time

	etcdKeyApi     etcd.KeysAPI
	httpsTransport *http.Transport
	httpsClient    *http.Client
)

type Test struct {
	Apps []Application
	User User
}

type User struct {
	Name              string
	Project           string
	KubeConfig        string
	Complete          bool
	Created           bool
	StartTime         time.Time
	UserCreateTime    time.Time
	ProjectCreateTime time.Time
	Error             error
}

type Application struct {
	Name           string
	User           *User
	Complete       bool
	Created        bool
	LastAppForUser bool
	StartTime      time.Time
	AppCreateTime  time.Time
	Error          error
	Pods           []Pod
}

type Pod struct {
	Name          string
	Key           string
	Status        string
	Complete      bool
	Created       bool
	StartTime     time.Time
	PodCreateTime time.Time
	Error         error
}

type EtcdPod struct {
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
	Metadata struct {
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
}

func main() {
	// Set the global variables that are defined with command-line flags
	flag.StringVar(&host, "host", "localhost", "OpenShift master hostname")
	flag.IntVar(&port, "port", 8443, "OpenShift master api port")
	flag.StringVar(&tmpDir, "tmp_dir", "/tmp/os-scale-test/", "Temporary directory to store files created during test. Directory will be deleted after test by default.")
	flag.StringVar(&adminKubeConfig, "admin_kubeconfig", "/openshift.local.config/master/admin.kubeconfig", "Kubeconfig file for the system:admin user.")
	flag.StringVar(&routerKubeConfig, "router_kubeconfig", "/openshift.local.config/master/openshift-router.kubeconfig", "Kubeconfig file for the system:router user.")
	flag.StringVar(&registryKubeConfig, "registry_kubeconfig", "/openshift.local.config/master/openshift-registry.kubeconfig", "Kubeconfig file for the system:registry user.")
	flag.StringVar(&appTemplate, "app_template", "", "Application template to use when creating applications. Must include the parameter APP_IDENTIFIER in resource names to allow multiple apps per user.")

	flag.StringVar(&caCrt, "ca_crt", "/openshift.local.config/master/ca.crt", "CA certificate for use with the master and etcd API's")
	flag.StringVar(&etcdClientCrt, "etcd_client_crt", "/openshift.local.config/master/master.etcd-client.crt", "Etcd client certificate")
	flag.StringVar(&etcdClientKey, "etcd_client_key", "/openshift.local.config/master/master.etcd-client.key", "Etcd client key")

	flag.IntVar(&numUsers, "users", 0, "Number of users to create.")
	flag.IntVar(&numAppsPerUser, "apps_per_user", 1, "Number of apps per user to create.")
	flag.IntVar(&numReplicas, "replicas", 3, "Number of pods per app.")
	flag.IntVar(&numThreads, "threads", 500, "Maximum threads to utilize.")
	flag.BoolVar(&verbose, "verbose", false, "Be verbose with output.")
	flag.BoolVar(&cleanup, "cleanup", true, "Delete temporary directory and files during test.")
	flag.Parse()

	startTime = time.Now()

	// Ensure temporary files are deleted
	if cleanup == true {
		defer cleanupTest()
	}

	// No error handling, as the setup function will bail where necessary.
	setupTest()

	// Here, we will want to loop through all the tests that were create with 'setupTest()'
	for i, test := range tests {
		// Create the user
		log.Println("Creating user", tests[i].User.Name)
		err := tests[i].User.Create()
		if err != nil {
			// If the user creation failed, mark the failure and move on to the next test.
			log.Println("Failed to create user", tests[i].User.Name, ":", err)
			tests[i].User.Error = err
			for x, _ := range test.Apps {
				tests[i].Apps[x].Error = errors.New("User could not be created, application was skipped.")
			}
			break
		}

		// Mark the user complete
		log.Println("User", test.User.Name, "creation complete.")
		tests[i].User.Created = true

		// Create the user's apps
		for x, app := range tests[i].Apps {
			log.Println("Creating application", app.Name, "for user", tests[i].User.Name)
			err := tests[i].Apps[x].Create()
			if err != nil {
				// If the app couldn't be created, bail
				log.Println("Failed to create app", app.Name, ":", err)
				app.Error = err
				break
			}

			err = tests[i].Apps[x].WatchPods()

			// Mark the app created
			log.Println("App", app.Name, "creation complete.")
			tests[i].Apps[x].Created = true

		}

	}

	log.Println("All resources created in etcd, waiting for pod creation to complete...")
	wg.Wait()
	summarizeTest()
}

func setupTest() {
	log.Println("Setting up test environment....")

	// If 0 or less users were specified to be created, bail.
	if numUsers < 1 {
		log.Fatal("Number of users to create must be specified and greater than 0.")
	}

	// Set the server hostname appropriately
	server = host + ":" + strconv.Itoa(port)
	err := os.Mkdir(tmpDir, 0755)
	if err != nil {
		if os.IsExist(err) == false {
			log.Fatal("Could not create temporary directory "+tmpDir+" for test due to:", err)
		}
	}

	// Create registry and router
	if registryKubeConfig != "" {
		err = createRegistry()
		if err != nil {
			log.Printf("Could not create docker registry: ", err)
		}
	}
	if routerKubeConfig != "" {
		err = createRouter()
		if err != nil {
			log.Printf("Could not create docker router, continuing anyway: ", err)
		}
	}

	// Create image streams
	err = createImageStreams()
	if err != nil {
		log.Printf("Could not create base image streams: ", err)
	}

	// Create json template
	templateFile, err = createTemplateJson()
	if err != nil {
		log.Fatal("Could not create application template: ", err)
	}

	// Get the https transport for talking at etcd
	httpsTransport, err := getHttpsTransport()
	if err != nil {
		log.Fatal("Could not create https transport for communication with etcd: ", err)
	}

	// Get the etcd api object
	etcdKeyApi, err = getEtcdClient(httpsTransport)
	if err != nil {
		log.Fatal("Could not create etcd key api client: ", err)
	}

	// Create user and application objects.
	for i := 0; i < numUsers; i++ {
		userName := "user" + strconv.Itoa(i)
		projectName := userName + "-project"
		kubeConfig := tmpDir + userName + ".kubeconfig"
		user := User{Name: userName, Project: projectName, KubeConfig: kubeConfig}
		var apps []Application
		for i := 0; i < numAppsPerUser; i++ {
			appName := user.Name + "-app" + strconv.Itoa(i)
			app := Application{Name: appName, User: &user}
			// If this is the last app for this user, mark it as such to track when a user has all apps completed
			if i == (numAppsPerUser - 1) {
				app.LastAppForUser = true
			}
			apps = append(apps, app)
		}
		test := Test{User: user, Apps: apps}
		tests = append(tests, test)
	}

}

// Create a user with 'oc login' and create a project for the user
func (user *User) Create() (err error) {
	config_arg := "--config=" + user.KubeConfig
	skip_tls_verify := "--insecure-skip-tls-verify=true"

	user.StartTime = time.Now()

	// Create the user
	// KubeConfig will be created with this command
	_, err = runCmd("oc", "login", "-u", user.Name, "-p", "pass", "--server="+server, skip_tls_verify, config_arg)
	if err != nil {
		return
	}

	user.UserCreateTime = time.Now()

	// Create the project
	description := "\"Created for openshift online scalability testing\""
	_, err = runCmd("oc", "new-project", user.Project, "--display-name="+user.Project, "--description="+description, config_arg)
	if err != nil {
		// Ignore this error, see https://github.com/openshift/origin/issues/4870
		// The project is still created, the failure is on the list after the project is created.
		if strings.Contains(err.Error(), "You are not a member of project \""+user.Project+"\".") {
			// The project doesn't get set in the kubeconfig though, so use 'oc project' to set it:
			tries := 0
			// Wait a total of 10 seconds, plus the time oc takes to run.
			// This is necessary one the system hits a high number of project resources.
			for tries < 20 {
				tries += 1
				time.Sleep(500 * time.Millisecond)
				_, err = runCmd("oc", "project", user.Project, config_arg)
				if err == nil {
					log.Printf("Was able to work around project issue for %s after %d tries", user.Project, tries)
					break
				}
			}
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	user.ProjectCreateTime = time.Now()

	return
}

// Set a user as complete and clear its files
func (user *User) Finish() {
	user.Complete = true
	if cleanup {
		err := os.Remove(user.KubeConfig)
		if err != nil {
			log.Println("Could not delete kubeconfig for user", user.Name)
			log.Println(err)
			if user.Error == nil {
				user.Error = err
			}
		}
	}
}

// Create an application
func (app *Application) Create() (err error) {
	config_arg := "--config=" + app.User.KubeConfig

	app.StartTime = time.Now()

	output, err := runCmd("oc", "process", "-f", templateFile, "-v", "APP_IDENTIFIER="+app.Name, config_arg)
	if err != nil {
		log.Println("Could not process application template", templateFile)
		return
	}

	fileName := tmpDir + app.Name + ".json"
	err = ioutil.WriteFile(fileName, []byte(output), 0644)
	if err != nil {
		log.Println("Could not write processed application json", fileName)
		return
	}
	defer func() {
		cleanupErr := os.Remove(fileName)
		if cleanupErr != nil {
			log.Println("Could not remove application json file:", fileName, cleanupErr)
		}
	}()

	_, err = runCmd("oc", "create", "-f", fileName, config_arg)
	if err != nil {
		log.Println("Could not create app from processed application json", fileName)
		return
	} else {
		app.AppCreateTime = time.Now()
	}

	return
}

func (app *Application) WatchPods() (err error) {
	config_arg := "--config=" + app.User.KubeConfig
	var pod_list []byte

	// I will need to get the list of pods we just created, probably using the selector that was returned from creating the resources.
	pod_list, err = runCmd("oc", "get", "pods", config_arg)
	if err != nil {
		log.Println("Could not get pods for application", app.Name)
		return err
	}
	pod_strings := strings.Split(string(pod_list), "\n")
	for i := range pod_strings {
		// Skip the header line
		if i == 0 || pod_strings[i] == "" {
			continue
		}
		podName := strings.Split(pod_strings[i], " ")[0]
		podKey := "/kubernetes.io/pods/" + app.User.Project + "/" + podName
		app.Pods = append(app.Pods, Pod{Name: podName, Key: podKey})
	}

	for i := range app.Pods {
		go app.Pods[i].Wait()
	}

	return
}

func (p *Pod) Wait() {
	wg.Add(1)
	defer wg.Done()

	var index uint64
	for i := 0; i < 3; i++ {
		getResp, err := getEtcdKey(p.Key)
		if err != nil {
			log.Println("Unable to get the pod key:", p.Key, err)
			time.Sleep(5 * time.Second)
		} else {
			index = getResp.ModifiedIndex
			break
		}
	}

	watcher, err := getEtcdWatcher(p.Key, index)
	if err != nil {
		log.Println("Error getting an etcd watcher:", err)
		return
	}

	p.Status = "Not started"
	for p.Status != "Running" {
		watchResp, _ := watcher.Next(context.Background())
		var podJson EtcdPod
		err = json.Unmarshal([]byte(watchResp.Node.Value), &podJson)
		if err != nil {
			log.Println("Error converting etcd pod key to json:", err)
			continue
		}

		p.Status = podJson.Status.Phase
		timeStr := strings.Split(podJson.Metadata.CreationTimestamp, "Z")[0]
		p.StartTime, err = time.Parse("2006-01-02T15:04:05", timeStr)
		if err != nil {
			log.Println("Unable to convert time:", timeStr, " ", err)
			continue
		}

		if p.Status == "Failed" {
			p.Error = errors.New("Failed")
			break
		}
	}
	log.Println("Pod", p.Name, "finished.")
	p.PodCreateTime = time.Now()
	p.Created = true
}

func getEtcdClient(t *http.Transport) (keyApi etcd.KeysAPI, err error) {
	ep := "https://" + host + ":4001"
	log.Println("Using etcd endpoint:", ep)
	config := etcd.Config{
		Endpoints: []string{ep},
		Transport: t,
	}

	c, err := etcd.New(config)
	if err != nil {
		return nil, err
	}

	keyApi = etcd.NewKeysAPI(c)
	return
}

// Get http response from etcd keyPath
func getEtcdKey(keyPath string) (key *etcd.Node, err error) {
	getOpts := etcd.GetOptions{Recursive: false}
	resp, err := etcdKeyApi.Get(context.Background(), keyPath, &getOpts)
	if err != nil {
		// TODO better error handling, see https://github.com/coreos/etcd/tree/master/client
		return &etcd.Node{}, err
	}

	key = resp.Node
	return
}

// To watch an etcd key, we instatiate a watcher, then call .Next() on it to block until a change occurs
func getEtcdWatcher(keyPath string, index uint64) (w etcd.Watcher, err error) {
	watchOpts := etcd.WatcherOptions{Recursive: false, AfterIndex: index}
	w = etcdKeyApi.Watcher(keyPath, &watchOpts)
	return
}

func getHttpsTransport() (t *http.Transport, err error) {
	cert, err := tls.LoadX509KeyPair(etcdClientCrt, etcdClientKey)
	if err != nil {
		log.Println("Could not load etcd client cert and key files:", err)
		return
	}

	caCert, err := ioutil.ReadFile(caCrt)
	if err != nil {
		log.Println("Could not load CA cert:", err)
		return
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: caCertPool}
	tlsConfig.BuildNameToCertificate()
	t = &http.Transport{
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   60 * time.Second,
		ResponseHeaderTimeout: time.Minute * 10,
		Proxy: nil,
	}
	//c = &http.Client{Transport: transport}
	return
}

// Create an internal docker registry
func createRegistry() (err error) {
	log.Println("Creating registry service account...")
	err = createServiceAccount("registry")
	if err != nil {
		log.Println(err)
		return
	}

	log.Println("Creating registry...")
	adminConfigArg := "--config=" + adminKubeConfig
	_, err = runCmd("oadm", "registry", adminConfigArg, "--credentials="+registryKubeConfig, "--service-account=registry")
	if err != nil {
		log.Println(err)
	}
	return
}

// Create a simple router
func createRouter() (err error) {
	log.Println("Creating router service account...")
	adminConfigArg := "--config=" + adminKubeConfig

	err = createServiceAccount("router")
	if err != nil {
		log.Println(err)
		return
	}

	log.Println("Creating router...")
	_, err = runCmd("oadm", "router", adminConfigArg, "--credentials="+routerKubeConfig, "--service-account=router")
	if err != nil {
		log.Println(err)
	}
	return
}

// Create imagestreams from external json
// These aren't used at all, remove? TODO
func createImageStreams() (err error) {
	log.Println("Creating image streams...")
	err = createResourceInOpenshift(imageStreamsUrl)
	if err != nil {
		return
	}
	err = createResourceInOpenshift(jbossImageStreamsUrl)
	return
}

func createResourceInOpenshift(resourceFile string) (err error) {
	adminConfigArg := "--config=" + adminKubeConfig

	_, err = runCmd("oc", "create", "-f", resourceFile, "-n", "openshift", adminConfigArg)
	if err != nil {
		log.Println(err)
	}
	return
}

// Do some oc get/replace magic to create service accounts
func createServiceAccount(name string) (err error) {
	adminConfigArg := "--config=" + adminKubeConfig

	// Create new service account json file
	json := `{"kind":"ServiceAccount","apiVersion":"v1","metadata":{"name":"` + name + `"}}`
	SCJsonFile := tmpDir + name + "_service_account.json"
	err = ioutil.WriteFile(SCJsonFile, []byte(json), 0644)
	if err != nil {
		log.Println(err)
		return
	}

	// Create new service account form json file
	_, err = runCmd("oc", "create", "-f", SCJsonFile, adminConfigArg)
	if err != nil {
		log.Println(err)
		return
	}

	// Export the "privileged" security context constraint
	output, err := runCmd("oc", "get", "scc", "privileged", "-o", "yaml", adminConfigArg)
	if err != nil {
		log.Println(err)
		return
	}

	// Add the service account to the "privileged" security context constraint output
	sccAccountLine := "- system:serviceaccount:default:" + name
	newScc := strings.Join([]string{string(output), sccAccountLine}, "\n")
	newSccFile := tmpDir + "modified_privileged_scc.yaml"
	err = ioutil.WriteFile(newSccFile, []byte(newScc), 0644)
	if err != nil {
		log.Println(err)
		return
	}

	// Patch the existing "privileged" security context constraint with the modified output
	_, err = runCmd("oc", "replace", "scc", "privileged", "-f", newSccFile, adminConfigArg)
	if err != nil {
		log.Println(err)
	}
	return
}

// Helper to run a system command
func runCmd(command string, args ...string) (stdout []byte, err error) {
	if verbose {
		log.Println("Running: ", command, args)
	}

	cmd := exec.Command(command, args...)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Println("Could not create stdoutPipe for command:", command)
		return nil, err
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		log.Println("Could not create stderrPipe for command:", command)
		return nil, err
	}

	err = cmd.Start()
	if err != nil {
		log.Println("Could not run command:", command)
		return nil, err
	}

	stdout, err = ioutil.ReadAll(stdoutPipe)
	if err != nil {
		log.Println("Couldn't read stdout of command", command)
	}

	stderr, err := ioutil.ReadAll(stderrPipe)
	if err != nil {
		log.Println("Couldn't read stderr of command", command)
	}

	err = cmd.Wait()
	if err != nil {
		log.Println("Command did not complete successfully:", cmd.Args)
	}

	if len(stderr) > 0 {
		// Set the stderr output of the command as the returned error
		stderrStr := strings.Trim(string(stderr), "\n") // some errors have multiple trailing newLines,
		if err != nil {
			err = errors.New(err.Error() + " : " + stderrStr)
		} else {
			err = errors.New(stderrStr)
		}
		if verbose {
			log.Println(stderrStr)
		}
	}

	if verbose {
		log.Printf("%s\n", stdout)
	}

	return
}

func summarizeTest() {
	var (
		totalLoginDuration time.Duration
		minLoginDuration   time.Duration
		maxLoginDuration   time.Duration

		totalProjectDuration time.Duration
		minProjectDuration   time.Duration
		maxProjectDuration   time.Duration

		totalAppCreateDuration time.Duration
		minAppCreateDuration   time.Duration
		maxAppCreateDuration   time.Duration

		totalPodCreateDuration time.Duration
		minPodCreateDuration   time.Duration
		maxPodCreateDuration   time.Duration

		successfulUsers []User
		successfulApps  []Application
		successfulPods  []Pod
		failedUsers     []User
		failedApps      []Application
		failedPods      []Pod
	)

	endTime := time.Now()
	timeFormatEx := "01/02/15 15:04:05"

	for i, test := range tests {
		if test.User.Created == true && test.User.Error == nil {
			successfulUsers = append(successfulUsers, test.User)

			userLoginDuration := test.User.UserCreateTime.Sub(test.User.StartTime)
			totalLoginDuration += userLoginDuration
			if userLoginDuration < minLoginDuration || i == 0 {
				minLoginDuration = userLoginDuration
			}
			if userLoginDuration > maxLoginDuration || i == 0 {
				maxLoginDuration = userLoginDuration
			}

			userProjectDuration := test.User.ProjectCreateTime.Sub(test.User.UserCreateTime)
			totalProjectDuration += userProjectDuration
			if userProjectDuration < minProjectDuration || i == 0 {
				minProjectDuration = userProjectDuration
			}
			if userProjectDuration > maxProjectDuration || i == 0 {
				maxProjectDuration = userProjectDuration
			}
			for _, app := range test.Apps {
				if app.Created == true && app.Error == nil {
					successfulApps = append(successfulApps, app)

					appCreateDuration := app.AppCreateTime.Sub(app.StartTime)
					totalAppCreateDuration += appCreateDuration
					if appCreateDuration < minAppCreateDuration || i == 0 {
						minAppCreateDuration = appCreateDuration
					}
					if appCreateDuration > maxAppCreateDuration || i == 0 {
						maxAppCreateDuration = appCreateDuration
					}
					for _, pod := range app.Pods {
						if pod.Created == true && pod.Error == nil {
							successfulPods = append(successfulPods, pod)

							podCreateDuration := pod.PodCreateTime.Sub(pod.StartTime)
							totalPodCreateDuration += podCreateDuration
							if podCreateDuration < minPodCreateDuration || i == 0 {
								minPodCreateDuration = podCreateDuration
							}
							if podCreateDuration > maxPodCreateDuration || i == 0 {
								maxPodCreateDuration = podCreateDuration
							}
						} else {
							failedPods = append(failedPods, pod)
						}
					}

				} else {
					failedApps = append(failedApps, app)
				}
			}
		} else {
			failedUsers = append(failedUsers, test.User)
			for _, app := range test.Apps {
				failedApps = append(failedApps, app)
			}
		}

	}

	avgAppCreateDuration := getAverageDuration(totalAppCreateDuration, len(successfulApps))
	var avgPodCreateDuration time.Duration
	if len(successfulPods) > 0 {
		avgPodCreateDuration = getAverageDuration(totalPodCreateDuration, len(successfulPods))
	} else {
		avgPodCreateDuration = 0
	}

	summary := `
Test Completed
---------------------------------------------
Test started:     ` + startTime.Format(timeFormatEx) + `
Test completed:   ` + endTime.Format(timeFormatEx) + `
Total test time:  ` + endTime.Sub(startTime).String() + `

OpenShift server: ` + server + `

Users created:   ` + strconv.Itoa(numUsers-len(failedUsers)) + `
Apps created:    ` + strconv.Itoa((numAppsPerUser*numUsers)-len(failedApps)) + `
Pods created:    ` + strconv.Itoa((numUsers*numAppsPerUser*numReplicas)-len(failedPods)) + `

Average resource creation time:       ` + avgAppCreateDuration.String() + `
Minimum resource creation time:       ` + minAppCreateDuration.String() + `
Maximum resource creation time:       ` + maxAppCreateDuration.String() + `

Average pod creation time:       ` + avgPodCreateDuration.String() + `
Minimum pod creation time:       ` + minPodCreateDuration.String() + `
Maximum pod creation time:       ` + maxPodCreateDuration.String() + `

Local Cpus used: ` + strconv.Itoa(runtime.NumCPU()) + `
Max threads:     ` + strconv.Itoa(numThreads)

	if (len(failedUsers) + len(failedApps) + len(failedPods)) > 0 {
		summary += `

Failed users:        ` + strconv.Itoa(len(failedUsers)) + `
Failed applications: ` + strconv.Itoa(len(failedApps)) + `
Failed pods:         ` + strconv.Itoa(len(failedPods))
	}

	fmt.Println(summary)

}

func getAverageDuration(total time.Duration, amount int) (avgDuration time.Duration) {
	avgNanoseconds := (total.Nanoseconds() / int64(amount))
	avgNanoString := strconv.FormatInt(avgNanoseconds, 10)
	avgDuration, err := time.ParseDuration(avgNanoString + "ns")
	if err != nil {
		log.Printf("Error getting average duration.")
	}
	return
}

// Remove all temporary files
func cleanupTest() {
	os.RemoveAll(tmpDir)
}

// Create json for application template
func createTemplateJson() (template string, err error) {
	if appTemplate != "" {
		template = appTemplate
		if _, err = os.Stat(appTemplate); err == nil {
			return template, nil
		} else {
			err = errors.New("Template file " + template + " could not be found.")
			return
		}
	}
	templateName := "ruby-multipleReplicas"
	template = tmpDir + templateName + ".json"
	// The below json will create a service, route, and replication controller(with a pod defined within).
	// The replication controller will be set to 0 replicas so that no pods are actually created.
	// This template is best used to only test the master api and etcd server.
	// TODO change template details to match the fact that this is the hello-openshift default application.
	json := `
{
  "kind": "Template",
  "apiVersion": "v1",
  "metadata": {
    "name": "` + templateName + `",
    "creationTimestamp": null,
    "annotations": {
      "description": "This template is used to create a simple ruby application in openshift origin v3 with no running pods",
      "iconClass": "icon-ruby",
      "tags": "instant-app,ruby,mysql"
    }
  },
  "objects": [
    {
      "kind": "Service",
      "apiVersion": "v1",
      "metadata": {
        "name": "${APP_IDENTIFIER}-service",
        "creationTimestamp": null
      },
      "spec": {
        "ports": [
          {
            "name": "web",
            "protocol": "TCP",
            "port": 5432,
            "targetPort": 8080,
            "nodePort": 0
          }
        ],
        "selector": {
          "name": "${APP_IDENTIFIER}-service"
        },
        "portalIP": "",
        "type": "ClusterIP",
        "sessionAffinity": "None"
      },
      "status": {
        "loadBalancer": {}
      }
    },
    {
      "kind": "Route",
      "apiVersion": "v1",
      "metadata": {
        "name": "${APP_IDENTIFIER}-route-edge",
        "creationTimestamp": null
      },
      "spec": {
        "to": {
          "kind": "Service",
          "name": "${APP_IDENTIFIER}-frontend"
        },
        "tls": {
          "termination": "edge",
          "certificate": "-----BEGIN CERTIFICATE-----\nMIIDIjCCAgqgAwIBAgIBATANBgkqhkiG9w0BAQUFADCBoTELMAkGA1UEBhMCVVMx\nCzAJBgNVBAgMAlNDMRUwEwYDVQQHDAxEZWZhdWx0IENpdHkxHDAaBgNVBAoME0Rl\nZmF1bHQgQ29tcGFueSBMdGQxEDAOBgNVBAsMB1Rlc3QgQ0ExGjAYBgNVBAMMEXd3\ndy5leGFtcGxlY2EuY29tMSIwIAYJKoZIhvcNAQkBFhNleGFtcGxlQGV4YW1wbGUu\nY29tMB4XDTE1MDExMjE0MTk0MVoXDTE2MDExMjE0MTk0MVowfDEYMBYGA1UEAwwP\nd3d3LmV4YW1wbGUuY29tMQswCQYDVQQIDAJTQzELMAkGA1UEBhMCVVMxIjAgBgkq\nhkiG9w0BCQEWE2V4YW1wbGVAZXhhbXBsZS5jb20xEDAOBgNVBAoMB0V4YW1wbGUx\nEDAOBgNVBAsMB0V4YW1wbGUwgZ8wDQYJKoZIhvcNAQEBBQADgY0AMIGJAoGBAMrv\ngu6ZTTefNN7jjiZbS/xvQjyXjYMN7oVXv76jbX8gjMOmg9m0xoVZZFAE4XyQDuCm\n47VRx5Qrf/YLXmB2VtCFvB0AhXr5zSeWzPwaAPrjA4ebG+LUo24ziS8KqNxrFs1M\nmNrQUgZyQC6XIe1JHXc9t+JlL5UZyZQC1IfaJulDAgMBAAGjDTALMAkGA1UdEwQC\nMAAwDQYJKoZIhvcNAQEFBQADggEBAFCi7ZlkMnESvzlZCvv82Pq6S46AAOTPXdFd\nTMvrh12E1sdVALF1P1oYFJzG1EiZ5ezOx88fEDTW+Lxb9anw5/KJzwtWcfsupf1m\nV7J0D3qKzw5C1wjzYHh9/Pz7B1D0KthQRATQCfNf8s6bbFLaw/dmiIUhHLtIH5Qc\nyfrejTZbOSP77z8NOWir+BWWgIDDB2//3AkDIQvT20vmkZRhkqSdT7et4NmXOX/j\njhPti4b2Fie0LeuvgaOdKjCpQQNrYthZHXeVlOLRhMTSk3qUczenkKTOhvP7IS9q\n+Dzv5hqgSfvMG392KWh5f8xXfJNs4W5KLbZyl901MeReiLrPH3w=\n-----END CERTIFICATE-----",
          "key": "-----BEGIN PRIVATE KEY-----\nMIICeAIBADANBgkqhkiG9w0BAQEFAASCAmIwggJeAgEAAoGBAMrvgu6ZTTefNN7j\njiZbS/xvQjyXjYMN7oVXv76jbX8gjMOmg9m0xoVZZFAE4XyQDuCm47VRx5Qrf/YL\nXmB2VtCFvB0AhXr5zSeWzPwaAPrjA4ebG+LUo24ziS8KqNxrFs1MmNrQUgZyQC6X\nIe1JHXc9t+JlL5UZyZQC1IfaJulDAgMBAAECgYEAnxOjEj/vrLNLMZE1Q9H7PZVF\nWdP/JQVNvQ7tCpZ3ZdjxHwkvf//aQnuxS5yX2Rnf37BS/TZu+TIkK4373CfHomSx\nUTAn2FsLmOJljupgGcoeLx5K5nu7B7rY5L1NHvdpxZ4YjeISrRtEPvRakllENU5y\ngJE8c2eQOx08ZSRE4TkCQQD7dws2/FldqwdjJucYijsJVuUdoTqxP8gWL6bB251q\nelP2/a6W2elqOcWId28560jG9ZS3cuKvnmu/4LG88vZFAkEAzphrH3673oTsHN+d\nuBd5uyrlnGjWjuiMKv2TPITZcWBjB8nJDSvLneHF59MYwejNNEof2tRjgFSdImFH\nmi995wJBAMtPjW6wiqRz0i41VuT9ZgwACJBzOdvzQJfHgSD9qgFb1CU/J/hpSRIM\nkYvrXK9MbvQFvG6x4VuyT1W8mpe1LK0CQAo8VPpffhFdRpF7psXLK/XQ/0VLkG3O\nKburipLyBg/u9ZkaL0Ley5zL5dFBjTV2Qkx367Ic2b0u9AYTCcgi2DsCQQD3zZ7B\nv7BOm7MkylKokY2MduFFXU0Bxg6pfZ7q3rvg8gqhUFbaMStPRYg6myiDiW/JfLhF\nTcFT4touIo7oriFJ\n-----END PRIVATE KEY-----",
          "caCertificate": "-----BEGIN CERTIFICATE-----\nMIIEFzCCAv+gAwIBAgIJALK1iUpF2VQLMA0GCSqGSIb3DQEBBQUAMIGhMQswCQYD\nVQQGEwJVUzELMAkGA1UECAwCU0MxFTATBgNVBAcMDERlZmF1bHQgQ2l0eTEcMBoG\nA1UECgwTRGVmYXVsdCBDb21wYW55IEx0ZDEQMA4GA1UECwwHVGVzdCBDQTEaMBgG\nA1UEAwwRd3d3LmV4YW1wbGVjYS5jb20xIjAgBgkqhkiG9w0BCQEWE2V4YW1wbGVA\nZXhhbXBsZS5jb20wHhcNMTUwMTEyMTQxNTAxWhcNMjUwMTA5MTQxNTAxWjCBoTEL\nMAkGA1UEBhMCVVMxCzAJBgNVBAgMAlNDMRUwEwYDVQQHDAxEZWZhdWx0IENpdHkx\nHDAaBgNVBAoME0RlZmF1bHQgQ29tcGFueSBMdGQxEDAOBgNVBAsMB1Rlc3QgQ0Ex\nGjAYBgNVBAMMEXd3dy5leGFtcGxlY2EuY29tMSIwIAYJKoZIhvcNAQkBFhNleGFt\ncGxlQGV4YW1wbGUuY29tMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA\nw2rK1J2NMtQj0KDug7g7HRKl5jbf0QMkMKyTU1fBtZ0cCzvsF4CqV11LK4BSVWaK\nrzkaXe99IVJnH8KdOlDl5Dh/+cJ3xdkClSyeUT4zgb6CCBqg78ePp+nN11JKuJlV\nIG1qdJpB1J5O/kCLsGcTf7RS74MtqMFo96446Zvt7YaBhWPz6gDaO/TUzfrNcGLA\nEfHVXkvVWqb3gqXUztZyVex/gtP9FXQ7gxTvJml7UkmT0VAFjtZnCqmFxpLZFZ15\n+qP9O7Q2MpsGUO/4vDAuYrKBeg1ZdPSi8gwqUP2qWsGd9MIWRv3thI2903BczDc7\nr8WaIbm37vYZAS9G56E4+wIDAQABo1AwTjAdBgNVHQ4EFgQUugLrSJshOBk5TSsU\nANs4+SmJUGwwHwYDVR0jBBgwFoAUugLrSJshOBk5TSsUANs4+SmJUGwwDAYDVR0T\nBAUwAwEB/zANBgkqhkiG9w0BAQUFAAOCAQEAaMJ33zAMV4korHo5aPfayV3uHoYZ\n1ChzP3eSsF+FjoscpoNSKs91ZXZF6LquzoNezbfiihK4PYqgwVD2+O0/Ty7UjN4S\nqzFKVR4OS/6lCJ8YncxoFpTntbvjgojf1DEataKFUN196PAANc3yz8cWHF4uvjPv\nWkgFqbIjb+7D1YgglNyovXkRDlRZl0LD1OQ0ZWhd4Ge1qx8mmmanoBeYZ9+DgpFC\nj9tQAbS867yeOryNe7sEOIpXAAqK/DTu0hB6+ySsDfMo4piXCc2aA/eI2DCuw08e\nw17Dz9WnupZjVdwTKzDhFgJZMLDqn37HQnT6EemLFqbcR0VPEnfyhDtZIQ==\n-----END CERTIFICATE-----"
        }
      },
      "status": {}
    },
    {
      "kind": "ReplicationController",
      "apiVersion": "v1",
      "metadata": {
        "name": "${APP_IDENTIFIER}-repcontroller"
      },
      "spec": {
        "replicas": ` + strconv.Itoa(numReplicas) + `,
        "selector": {
          "name": "${APP_IDENTIFIER}-pod"
        },
        "template": {
          "metadata": {
            "labels": {
              "name": "${APP_IDENTIFIER}-pod"
            }
          },
          "spec": {
            "restartPolicy": "Always",
            "containers": [
              {
                "image": "openshift/hello-openshift:latest",
                "name": "helloworld",
								"resources": {
									"requests": {
										"memory": "128Mi",
										"cpu": "20m"
									},
									"limits": {
										"memory": "512Mi",
										"cpu": "40m"
									}
								},
                "ports": [
                  {
                    "containerPort": 8080,
                    "protocol": "TCP"
                  }
                ]
              }
            ]
          }
        }
      }
    }
  ],
  "parameters": [
    {
      "name": "APP_IDENTIFIER",
      "description": "String to prepend to the names of resource objects",
      "value": "app[0-9]{4}"
    }
  ],
  "labels": {
    "template": "` + templateName + `"
  }
}
`
	err = ioutil.WriteFile(template, []byte(json), 0644)
	if err != nil {
		log.Printf("Could not write template json", template)
	}

	return
}
