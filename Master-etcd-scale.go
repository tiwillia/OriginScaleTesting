package main

/*
INSTRUCTIONS:
- The test can be run on the same machine as the openshift server, but an accurate test should be run from a client machine. If run from the same machine as the openshift server, the test uses the default configuration locations (/openshift.local.config/) and the only arguments that need to be provided are --users, --apps_per_user, and --threads

- To run from a client machine, you will need a configured admin.kubeconfig file. This is usually already generated for a devenv. Below is an example run:
	$ test-online-scale --admin_kubeconfig=/tmp/admin.kubeconfig --host=example.com --users 100 --apps_per_user 5 --threads 100

- If a registry and/or router are necessary, kubeconfig files for those will also need to be provided:

	$ test-online-scale --admin_kubeconfig=/tmp/admin.kubeconfig --registry_kubeconfig=/tmp/registry.kubeconfig --router_kubeconfig=/tmp/router.kubeconfig --host=example.com --users 100 --apps_per_user 5 --threads 100

- The test will create csv files in the working directory after completion, as well as reporting a summary of the test. Note that if there are errors, the output can be quite long. Ensure the output is either sent to a file or the scrollback on your terminal is unlimited.

- Note that the test itself will need a myriad of open file descriptors. Ensure the nofile limits in /etc/security/limits.conf have been increased for this user.

- Note that the test will create many open file descriptors on the openshift server. Ensure the nofile limits in /etc/security/limits.conf have been increased, as well as the file limit in the systemd service file if said file exists.

*/

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	imageStreamsUrl      = "https://raw.githubusercontent.com/openshift/origin/master/examples/image-streams/image-streams-rhel7.json"
	jbossImageStreamsUrl = "https://raw.githubusercontent.com/jboss-openshift/application-templates/master/jboss-image-streams.json"
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
	userStart          int
	numAppsPerUser     int
	numThreads         int
	verbose            bool
	cleanup            bool

	server        string
	caCrt         string
	etcdClientCrt string
	etcdClientKey string
	templateFile  string

	wg         sync.WaitGroup
	tests      []Test
	startTime  time.Time
	httpClient *http.Client
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
	User           *User // Needed to reference kubeconfig.
	Complete       bool
	Created        bool
	LastAppForUser bool
	StartTime      time.Time
	AppCreateTime  time.Time
	Error          error
}

type EtcdKey struct {
	Node struct {
		CreatedIndex  int    `json:"createdIndex"`
		ModifiedIndex int    `json:"modifiedIndex"`
		Value         string `json:"value"`
	} `json:"node"`
}

func main() {
	flag.StringVar(&host, "host", "localhost", "OpenShift master hostname")
	flag.IntVar(&port, "port", 8443, "OpenShift master api port")
	flag.StringVar(&tmpDir, "tmp_dir", "/tmp/os-scale-test/", "Temporary directory to store files created during test. Directory will be deleted after test by default.")
	flag.StringVar(&adminKubeConfig, "admin_kubeconfig", "/openshift.local.config/master/admin.kubeconfig", "Kubeconfig file for the system:admin user.")
	flag.StringVar(&routerKubeConfig, "router_kubeconfig", "/openshift.local.config/master/openshift-router.kubeconfig", "Kubeconfig file for the system:router user.")
	flag.StringVar(&registryKubeConfig, "registry_kubeconfig", "/openshift.local.config/master/openshift-registry.kubeconfig", "Kubeconfig file for the system:registry user.")
	flag.StringVar(&appTemplate, "app_template", "", "Application template to use when creating applications. Must include the parameter APP_IDENTIFIER in resource names to allow multiple apps per user.")

	flag.IntVar(&numUsers, "users", 0, "Number of users to create.")
	flag.IntVar(&userStart, "startnum", 0, "Identifying user number to start from")
	flag.IntVar(&numAppsPerUser, "apps_per_user", 1, "Number of per user to create.")
	flag.IntVar(&numThreads, "threads", 500, "Maximum threads to utilize.")
	flag.BoolVar(&verbose, "verbose", false, "Be verbose with output.")
	flag.BoolVar(&cleanup, "cleanup", true, "Delete temporary directory and files during test.")
	flag.Parse()
	startTime = time.Now()

	// Ensure temporary files are deleted
	if cleanup == true {
		defer cleanupTest()
	}

	// Set up the test environment
	setupTest()

	// Create channels
	appChan := make(chan *Application, numUsers*numAppsPerUser)
	userChan := make(chan *Test, numUsers)
	createdUserChan := make(chan *Test, numUsers)
	userThreadCompleteChan := make(chan bool)

	// After this point, catch SIGINT to always report the test summary.
	// TODO this doesn't work
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	go func() {
		<-sigChan
		if cleanup == true {
			cleanupTest()
		}
		summarizeTest()
		os.Exit(1)
	}()

	// Start goroutine that will pass tests to channels
	go testDelegator(userChan, createdUserChan, appChan)

	// 10% of threads will be dedicated to creating users until all users are complete, then
	// they will be stopped and replaced with threads dedicated to creating apps
	userThreadCount := 1
	if numThreads > 10 {
		userThreadCount = numThreads / 10
	}
	appThreadCount := numThreads - userThreadCount

	// Create worker threads that watch the userChan and appChan
	for i := 0; i < userThreadCount; i++ {
		wg.Add(1)
		go userWorker(userChan, createdUserChan, userThreadCompleteChan)
	}
	for i := 0; i < appThreadCount; i++ {
		wg.Add(1)
		go appWorker(appChan)
	}

	// As user creation finishes, replace userWorker goroutines with
	// appWorker goroutines
	finishedUserThreads := 0
	for finishedUserThreads < userThreadCount {
		<-userThreadCompleteChan // blocks until a userWorker goroutine is complete
		finishedUserThreads++
		wg.Add(1)
		go appWorker(appChan)
	}

	// At this point, all users and apps should be complete
	// Ensure channels are closed
	// Wait for a bit first, to avoid a race condition where the channel is closed before the final app is sent.
	time.Sleep(20 * time.Second)
	close(createdUserChan)
	close(appChan)

	// Wait for the remaining applications to complete
	wg.Wait()

	summarizeTest()
}

// Send the tests on the proper channels
// Don't send applications until respective users are created
func testDelegator(userChan chan *Test, createdUserChan chan *Test, appChan chan *Application) {
	// For each test, send the user to be created
	for i := 0; i < numUsers; i++ {
		userChan <- &tests[i]
	}
	close(userChan)

	for test := range createdUserChan {
		for i := 0; i < len(test.Apps); i++ {
			appChan <- &test.Apps[i]
		}
	}
}

// Worker, meant to be run in a goroutine, that creates users
func userWorker(userChan chan *Test, createdUserChan chan *Test, userThreadCompleteChan chan bool) {
	defer wg.Done()
	for test := range userChan {
		log.Println("Creating user:", test.User.Name)
		err := test.User.Create()
		if err != nil {
			log.Println("Failed to create user", test.User.Name, ":", err)
			test.User.Error = err
			for i, _ := range test.Apps {
				test.Apps[i].Error = errors.New("User could not be created, application was skipped.")
			}
		} else {
			log.Println("User", test.User.Name, "creation complete.")
			test.User.Created = true
			createdUserChan <- test
		}
	}
	// Let others know that no more users are left in the channel and it has been closed
	userThreadCompleteChan <- true
}

// Worker, meant to be run in a goroutine, that creates applications
func appWorker(appChan chan *Application) {
	defer wg.Done()
	for app := range appChan {
		log.Println("Creating app:", app.Name)
		err := app.Create()
		if err != nil {
			log.Println("Failed to create app", app.Name, ":", err)
			app.Error = err
		} else {
			log.Println("App", app.Name, "creation complete.")
			app.Created = true
			// TODO this is where we would watch etcd if needed
		}
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
		/* This issue has been resolved - ignore the workaround
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
		*/
		return err
		//}
	}
	user.ProjectCreateTime = time.Now()

	return
}

// Set a user as complete and clear its files
func (user *User) Finish() {
	user.Complete = true
	if cleanup == true {
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

	var output []byte
	var finished bool
	finished = false

	for finished == false {
		output, err = runCmd("oc", "process", "-f", templateFile, "-v", "APP_IDENTIFIER="+app.Name, config_arg)
		if err != nil {
			log.Println("Could not process application template", templateFile)
		} else {
			fileName := tmpDir + app.Name + ".json"
			err = ioutil.WriteFile(fileName, []byte(output), 0644)
			if err != nil {
				log.Println("Could not write processed application json", fileName)
			} else {
				defer func() {
					if cleanup == true {
						cleanupErr := os.Remove(fileName)
						if cleanupErr != nil {
							log.Println("Could not remove application json file:", fileName, cleanupErr)
						}
					}
				}()

				_, err = runCmd("oc", "create", "-f", fileName, config_arg)
			}
		}

		if err == nil {
			finished = true
		} else {
			if strings.Contains(err.Error(), "in project") {
				log.Println(err)
				log.Println("Could not create app from processed application json, retrying...")
				err = nil
				time.Sleep(5 * time.Second)
			} else {
				log.Println(err)
				log.Println("App creation unable to be retryed, moving on...")
				finished = true
				break
			}
		}
	}

	app.AppCreateTime = time.Now()
	return
}

// Get http response from etcd keyPath
// * Responsibility of the caller to close response body *
func getKeyFromEtcd(keyPath string) (body []byte, statusCode int, err error) {
	urlStr := "https://127.0.0.1:4001/v2/keys/" + keyPath
	body, statusCode, err = httpsGet(urlStr, nil)
	return
}

// Wait for an etcd key to change
// An index should only be provided if an OLD change is watched
// Providing an index will always cause the request to return immediately, rather than waiting, even if the key is higher than the current ModifiedIndex.
func waitForKeyFromEtcd(keyPath string, index int) (body []byte, statusCode int, err error) {
	urlStr := "https://127.0.0.1:4001/v2/keys/" + keyPath
	// A timeout is implemented on the tranport, so no timeout is needed here
	options := map[string]string{"wait": "true", "waitIndex": strconv.Itoa(index)}
	body, statusCode, err = httpsGet(urlStr, options)
	return
}

// Will always return an error unless http code 200
func httpsGet(urlStr string, options map[string]string) (body []byte, statusCode int, err error) {
	if options != nil {
		values := url.Values{}
		for k, v := range options {
			values.Set(k, v)
		}
		urlStr += "?" + values.Encode()
	}

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		log.Println("Could not create https request:", err)
		return nil, 0, err
	}

	resp, err := httpClient.Do(req)
	if err == nil && resp.StatusCode != 200 {
		err = errors.New(resp.Status)
	}
	defer resp.Body.Close()

	statusCode = resp.StatusCode
	body, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println("Could not read from response body:", err)
	}

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

func getHttpsClient() (client *http.Client, err error) {
	cert, err := tls.LoadX509KeyPair(etcdClientCrt, etcdClientKey)
	if err != nil {
		log.Println("Could not load etcd client cert and key files:", err)
		return nil, err
	}

	caCert, err := ioutil.ReadFile(caCrt)
	if err != nil {
		log.Println("Could not load CA cert:", err)
		return nil, err
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: caCertPool}
	tlsConfig.BuildNameToCertificate()
	transport := &http.Transport{
		TLSClientConfig:       tlsConfig,
		ResponseHeaderTimeout: time.Minute * 10,
	}
	client = &http.Client{Transport: transport}
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

// Summarize the whole test and output details
/* The following should be printed by this method:
Total time for the whole test
Average time for user login
Average time for project creation
Average time for app creation
Average time for each resource to create
*/
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

		successfulUsers []User
		successfulApps  []Application
		failedUsers     []User
		failedApps      []Application
	)

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

	avgLoginDuration := getAverageDuration(totalLoginDuration, len(successfulUsers))
	avgProjectDuration := getAverageDuration(totalProjectDuration, len(successfulUsers))
	avgAppCreateDuration := getAverageDuration(totalAppCreateDuration, len(successfulApps))

	endTime := time.Now()
	timeFormatEx := "01/02/15 15:04:05"

	summary := `
Test completed
---------------------------------------------
Test started:     ` + startTime.Format(timeFormatEx) + `
Test completed:   ` + endTime.Format(timeFormatEx) + `
Total test time:  ` + endTime.Sub(startTime).String() + `

OpenShift server: ` + server + `

Users created:   ` + strconv.Itoa(numUsers-len(failedUsers)) + `
Apps created:    ` + strconv.Itoa((numAppsPerUser*numUsers)-len(failedApps)) + `

Average 'oc login' time:       ` + avgLoginDuration.String() + `
Minimum 'oc login' time:       ` + minLoginDuration.String() + `
Maximum 'oc login' time:       ` + maxLoginDuration.String() + `

Average 'oc new-project' time: ` + avgProjectDuration.String() + `
Minimum 'oc new-project' time: ` + minProjectDuration.String() + `
Maximum 'oc new-project' time: ` + maxProjectDuration.String() + `

Average 'oc create' time:       ` + avgAppCreateDuration.String() + `
Minimum 'oc create' time:       ` + minAppCreateDuration.String() + `
Maximum 'oc create' time:       ` + maxAppCreateDuration.String() + `

Local Cpus used: ` + strconv.Itoa(runtime.NumCPU()) + `
Max threads:     ` + strconv.Itoa(numThreads)

	if len(failedUsers) > 0 || len(failedApps) > 0 {
		summary += `

Failed users:        ` + strconv.Itoa(len(failedUsers)) + `
Failed applications: ` + strconv.Itoa(len(failedApps)) + `

Errors:
`
		for _, user := range failedUsers {
			summary += (user.Name + ": " + fmt.Sprintln(user.Error))
		}

		skippedApps := 0
		for _, app := range failedApps {
			if app.Error.Error() == "User could not be created, application was skipped." {
				skippedApps++
			} else {
				if app.Created == true && app.Error != nil {
					summary += (app.Name + ": " + fmt.Sprintln(app.Error))
				}
			}
		}
		if skippedApps > 0 {
			summary += strconv.Itoa(skippedApps) + " applications skipped since the relevant user could not be created."
		}
	} else {
		summary += "\n\nNo failures."
	}

	summary += "\n---------------------------------------------"
	fmt.Println(summary)
	err := exportOcCsv(successfulUsers, successfulApps)
	if err != nil {
		fmt.Println("Could not export CSV files:", err)
	}
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

// Export collected command times to csv files
// This doesn't use the global app and user list, but instead takes them as
// arguments so that only the successful commands can be passed in
func exportOcCsv(userList []User, appList []Application) (err error) {
	userHeaders := []string{"Users", "Time(seconds)"}
	appHeaders := []string{"Apps", "Time(seconds)"}
	var loginRows [][]string
	var projectRows [][]string
	var appRows [][]string
	loginRows = append(loginRows, userHeaders)
	projectRows = append(projectRows, userHeaders)
	appRows = append(appRows, appHeaders)

	for _, user := range userList {
		loginDuration := user.UserCreateTime.Sub(user.StartTime)
		loginDurationString := strconv.FormatFloat(loginDuration.Seconds(), 'f', 2, 64)
		loginRows = append(loginRows, []string{user.Name, loginDurationString})

		projectDuration := user.ProjectCreateTime.Sub(user.UserCreateTime)
		projectDurationString := strconv.FormatFloat(projectDuration.Seconds(), 'f', 2, 64)
		projectRows = append(projectRows, []string{user.Name, projectDurationString})
	}

	for _, app := range appList {

		appDuration := app.AppCreateTime.Sub(app.StartTime)
		appDurationString := strconv.FormatFloat(appDuration.Seconds(), 'f', 2, 64)
		appRows = append(appRows, []string{app.Name, appDurationString})
	}

	err = writeCsv("TestLoginResults.csv", loginRows)
	if err != nil {
		log.Println(err)
		return
	}
	err = writeCsv("TestNewProjectResults.csv", projectRows)
	if err != nil {
		log.Println(err)
		return
	}
	err = writeCsv("TestNewAppResults.csv", appRows)
	if err != nil {
		log.Println(err)
		return
	}

	return
}

// Write rows of string slices to a csv file
func writeCsv(filename string, rows [][]string) (err error) {
	file, err := os.Create(filename) // Create truncates if the file already exists.
	if err != nil {
		log.Printf("Could not create csv file.")
		return
	}
	defer file.Close()

	csvWriter := csv.NewWriter(file)
	for _, row := range rows {
		err = csvWriter.Write(row)
		if err != nil {
			log.Printf("Unable to write to csv file", filename)
			return
		}
	}
	csvWriter.Flush()
	return
}

// Remove all temporary files
func cleanupTest() {
	if cleanup == true {
		os.RemoveAll(tmpDir)
	}
}

/* This function does the following:
creates a temp directory
creates a docker registry
creates a router
creates image streams
creates application templates
creates list of "tests" to be run
*/
func setupTest() {
	log.Println("Setting up test environment...")

	if numUsers < 1 {
		log.Fatal("Number of users to create must be specified and greater than 0.")
	}
	if numAppsPerUser < 0 {
		log.Fatal("Number of apps per user must be 0 or a positive integer.")
	}
	if appTemplate == "" {
		log.Fatal("An application template must be specified.")
	}

	server = host + ":" + strconv.Itoa(port)
	/*	caCrt = masterConfigDir + "ca.crt"
		etcdClientCrt = masterConfigDir + "master.etcd-client.crt"
		etcdClientKey = masterConfigDir + "master.etcd-client.key"
	TODO this etcd stuff should probably be defined if we are goint to actually use any of the etcd watchers*/

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

	err = createImageStreams()
	if err != nil {
		log.Printf("Could not create base image streams: ", err)
	}

	templateFile, err = createTemplateJson()
	if err != nil {
		log.Fatal("Could not create application template: ", err)
	}

	// Create user and application objects
	for i := userStart; i < numUsers+userStart; i++ {
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

// Create json for application template
func createTemplateJson() (template string, err error) {
	template = appTemplate
	if _, err = os.Stat(appTemplate); err == nil {
		return template, nil
	} else {
		err = errors.New("Template file " + template + " could not be found.")
		return
	}

	return
}
