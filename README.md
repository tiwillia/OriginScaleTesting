# Origin scalability testing scripts
-----
These are one-off scripts developed to perform specific scalability tests for Origin.

###Master-etcd-scale.go:
  This script's purpose is to scale an openshift origin system using a pre-defined template and assuming that each x users will be created with one project each. 
  ```
    -admin_kubeconfig string
    	Kubeconfig file for the system:admin user. (default "/openshift.local.config/master/admin.kubeconfig")
  -app_template string
    	Application template to use when creating applications. Must include the parameter APP_IDENTIFIER in resource names to allow multiple apps per user.
  -apps_per_user int
    	Number of per user to create. (default 1)
  -cleanup
    	Delete temporary directory and files during test. (default true)
  -host string
    	OpenShift master hostname (default "localhost")
  -port int
    	OpenShift master api port (default 8443)
  -registry_kubeconfig string
    	Kubeconfig file for the system:registry user. (default "/openshift.local.config/master/openshift-registry.kubeconfig")
  -router_kubeconfig string
    	Kubeconfig file for the system:router user. (default "/openshift.local.config/master/openshift-router.kubeconfig")
  -startnum int
    	Identifying user number to start from
  -threads int
    	Maximum threads to utilize. (default 500)
  -tmp_dir string
    	Temporary directory to store files created during test. Directory will be deleted after test by default. (default "/tmp/os-scale-test/")
  -users int
    	Number of users to create.
  -verbose
    	Be verbose with output.
```

Note that this test requires a template be provided. An example template has been provided titled 'Master-template_example.json'


###Node-PodWatcher.go
  The purpose of this script is to determine possible pod limits for a node and to determine the difference in time pods take to create depending on how many pods a node currently has. This script is a derivitive of the `Master-etcd-scale.go`. There is no concurrency. Instead, templates will be created one-at-a-time. Pods created by each template will be watched in etcd. The time it takes for the pod to go from being created in etcd until it is in RUNNING state is recorded. 
  ```
    -admin_kubeconfig string
    	Kubeconfig file for the system:admin user. (default "/openshift.local.config/master/admin.kubeconfig")
  -app_template string
    	Application template to use when creating applications. Must include the parameter APP_IDENTIFIER in resource names to allow multiple apps per user.
  -apps_per_user int
    	Number of apps per user to create. (default 1)
  -ca_crt string
    	CA certificate for use with the master and etcd API's (default "/openshift.local.config/master/ca.crt")
  -cleanup
    	Delete temporary directory and files during test. (default true)
  -etcd_client_crt string
    	Etcd client certificate (default "/openshift.local.config/master/master.etcd-client.crt")
  -etcd_client_key string
    	Etcd client key (default "/openshift.local.config/master/master.etcd-client.key")
  -host string
    	OpenShift master hostname (default "localhost")
  -port int
    	OpenShift master api port (default 8443)
  -registry_kubeconfig string
    	Kubeconfig file for the system:registry user. (default "/openshift.local.config/master/openshift-registry.kubeconfig")
  -replicas int
    	Number of pods per app. (default 3)
  -router_kubeconfig string
    	Kubeconfig file for the system:router user. (default "/openshift.local.config/master/openshift-router.kubeconfig")
  -threads int
    	Maximum threads to utilize. (default 500)
  -tmp_dir string
    	Temporary directory to store files created during test. Directory will be deleted after test by default. (default "/tmp/os-scale-test/")
  -users int
    	Number of users to create.
  -verbose
    	Be verbose with output.
  ```

This test creates from a template, but the template is embedded in the script due to the need to specify a specific number of replicas and this test's reliance on pod creation.
