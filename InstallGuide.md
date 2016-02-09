Atomic OpenShift Quick Install Guide
====================================

This is an abbreviated guide for installing Atomic OpenShift. It was tested on
AWS with RHEL 7.2.

For performance and scalability testing, use of the **m4.xlarge** or
**m4.2xlarge** instance types is recommended.

A more complete guide can be found in the official Red Hat OpenShift Enterprise
documentation here:

https://access.redhat.com/documentation/en/openshift-enterprise/version-3.1/installation-and-configuration/#prerequisites-1

Subscription Registration
-------------------------

Use `subscription-manager` to register your RHEL instance:

````
# subscription-manager register
# subscription-manager list --available
# subscription-manager attach --pool xxxxxx
# subscription-manager repos --disable="*"
# subscription-manager repos \
    --enable="rhel-7-server-rpms" \
    --enable="rhel-7-server-extras-rpms" \
    --enable="rhel-7-server-ose-3.1-rpms" \
    --enable="rhel-7-server-optional-rpms"
````

System Update
-------------

Update your system to the latest packages. It is likely that a new kernel has
been installed, so be sure to restart your system as well to take advantage of
that.

````
# yum update
# shutdown -r now
````

Required Packages
-----------------

Run the following installation command. You may also install other packages
at this time, e.g. a preferred editor or other tools.

````
# yum install wget git net-tools bind-utils iptables-services bridge-utils bash-completion atomic-openshift-utils docker vim golang && \
    yum install https://dl.fedoraproject.org/pub/epel/7/x86_64/e/epel-release-7-5.noarch.rpm && \
    yum install htop golang
````

Installing
----------

Modify your `/etc/sysconfig/docker` file to enable pulling packages from the
local registry without requiring a trusted SSL certificate:

````
OPTIONS='--selinux-enabled --insecure-registry 172.30.0.0/16'
````

Follow the Docker storage setup instructions. If you are just using an extra
volume in EC2, the following snippet should suffice. You may skip this step
if your tests do not require actually creating containers.

````
# cat <<EOF > /etc/sysconfig/docker-storage-setup
DEVS=/dev/xvdb
VG=docker-vg
EOF
# docker-storage-setup
````

Generate an SSH key (you can use an empty passphrase; just hit enter a few
times) and add the public key to localhost. Make sure to test the key quickly,
as it is used by the installer.

````
# ssh-keygen
# cat ~/.ssh/id_rsa.pub >>~/.ssh/authorized_keys
# ssh localhost
````

Run the quick installation using the public hostname.

````
# atomic-openshift-installer install
````

Modify the settings (`/etc/origin/master/master-config.yaml`) to enable
authentication with any password, by changing the identityProvider kind
to *AllowAllPasswordIdentityProvider*. You will then be able to run
`oc login` with any username/password combination.

````
  identityProviders:
  - challenge: true
    login: true
    mappingMethod: claim
    name: anypassword
    provider:
      apiVersion: v1
      kind: AllowAllPasswordIdentityProvider
````

Restart atomic-openshift-master using: `systemctl restart atomic-openshift-master`
