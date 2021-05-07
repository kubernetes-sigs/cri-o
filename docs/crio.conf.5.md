% crio.conf(5) Kubernetes Container Runtime Daemon for Open Container Initiative Containers
% Aleksa Sarai
% OCTOBER 2016

# NAME
crio.conf - configuration file of the CRI-O OCI Kubernetes Container Runtime daemon

# DESCRIPTION
The CRI-O configuration file specifies all of the available configuration options and command-line flags for the [crio(8) OCI Kubernetes Container Runtime daemon][crio], but in a TOML format that can be more easily modified and versioned.

CRI-O supports partial configuration reload during runtime, which can be done by sending SIGHUP to the running process. Currently supported options in `crio.conf` are explicitly marked with 'This option supports live configuration reload'.

The containers-registries.conf(5) file can be reloaded as well by sending SIGHUP to the `crio` process.

The default crio.conf is located at /etc/crio/crio.conf.

# FORMAT
The [TOML format][toml] is used as the encoding of the configuration file. Every option and subtable listed here is nested under a global "crio" table. No bare options are used. The format of TOML can be simplified to:

    [table]
    option = value

    [table.subtable1]
    option = value

    [table.subtable2]
    option = value

## CRIO TABLE
CRI-O reads its storage defaults from the containers-storage.conf(5) file located at /etc/containers/storage.conf. Modify this storage configuration if you want to change the system's defaults. If you want to modify storage just for CRI-O, you can change the storage configuration options here.

**root**="/var/lib/containers/storage"
  Path to the "root directory". CRI-O stores all of its data, including containers images, in this directory.

**runroot**="/var/run/containers/storage"
  Path to the "run directory". CRI-O stores all of its state in this directory.

**storage_driver**="overlay"
  Storage driver used to manage the storage of images and containers. Please refer to containers-storage.conf(5) to see all available storage drivers.

**storage_option**=[]
  List to pass options to the storage driver. Please refer to containers-storage.conf(5) to see all available storage options.

**log_dir**="/var/log/crio/pods"
  The default log directory where all logs will go unless directly specified by the kubelet. The log directory specified must be an absolute directory.

**version_file**="/var/run/crio/version"
  Location for CRI-O to lay down the temporary version file.
  It is used to check if crio wipe should wipe containers, which should
  always happen on a node reboot

**version_file_persist**="/var/lib/crio/version"
  Location for CRI-O to lay down the persistent version file.
  It is used to check if crio wipe should wipe images, which should
  only happen when CRI-O has been upgraded

**internal_wipe**=false
  Whether CRI-O should wipe containers after a reboot and images after an upgrade when the server starts.
  If set to false, one must run `crio wipe` to wipe the containers and images in these situations.

**clean_shutdown_file**="/var/lib/crio/clean.shutdown"
  Location for CRI-O to lay down the clean shutdown file.
  It is used to check whether crio had time to sync before shutting down.
  If not found, crio wipe will clear the storage directory.

## CRIO.API TABLE
The `crio.api` table contains settings for the kubelet/gRPC interface.

**listen**="/var/run/crio/crio.sock"
  Path to AF_LOCAL socket on which CRI-O will listen.

**stream_address**="127.0.0.1"
  IP address on which the stream server will listen.

**stream_port**="0"
  The port on which the stream server will listen. If the port is set to "0", then CRI-O will allocate a random free port number.

**stream_enable_tls**=false
  Enable encrypted TLS transport of the stream server.

**stream_idle_timeout**=""
  Length of time until open streams terminate due to lack of activity.

**stream_tls_cert**=""
  Path to the x509 certificate file used to serve the encrypted stream. This file can change and CRI-O will automatically pick up the changes within 5 minutes.

**stream_tls_key**=""
  Path to the key file used to serve the encrypted stream. This file can change and CRI-O will automatically pick up the changes within 5 minutes.

**stream_tls_ca**=""
  Path to the x509 CA(s) file used to verify and authenticate client communication with the encrypted stream. This file can change and CRI-O will automatically pick up the changes within 5 minutes.

**grpc_max_send_msg_size**=16777216
  Maximum grpc send message size in bytes. If not set or <=0, then CRI-O will default to 16 * 1024 * 1024.

**grpc_max_recv_msg_size**=16777216
  Maximum grpc receive message size. If not set or <= 0, then CRI-O will default to 16 * 1024 * 1024.

## CRIO.RUNTIME TABLE
The `crio.runtime` table contains settings pertaining to the OCI runtime used and options for how to set up and manage the OCI runtime.

**default_runtime**="runc"
  The _name_ of the OCI runtime to be used as the default.

**default_ulimits**=[]
  A list of ulimits to be set in containers by default, specified as "<ulimit name>=<soft limit>:<hard limit>", for example:"nofile=1024:2048". If nothing is set here, settings will be inherited from the CRI-O daemon.

**no_pivot**=false
  If true, the runtime will not use `pivot_root`, but instead use `MS_MOVE`.

**decryption_keys_path**="/etc/crio/keys/"
  Path where the keys required for image decryption are located

**conmon**=""
  Path to the conmon binary, used for monitoring the OCI runtime. Will be searched for using $PATH if empty.

**conmon_cgroup**="system.slice"
  Cgroup setting for conmon

**conmon_env**=["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"]
  Environment variable list for the conmon process, used for passing necessary environment variables to conmon or the runtime.

**default_env**=[]
  Additional environment variables to set for all the containers. These are overridden if set in the container image spec or in
the container runtime configuration.

**selinux**=false
  If true, SELinux will be used for pod separation on the host.

**seccomp_profile**=""
  Path to the seccomp.json profile which is used as the default seccomp profile for the runtime. If not specified, then the internal default seccomp profile will be used.

**seccomp_use_default_when_empty**=false
  Changes the meaning of an empty seccomp profile.  By default (and according to CRI spec), an empty profile means unconfined.
  This option tells CRI-O to treat an empty profile as the default profile, which might increase security.

**apparmor_profile**=""
  Used to change the name of the default AppArmor profile of CRI-O. The default profile name is "crio-default".

**blockio_config_file**=""
  Path to the blockio class configuration file for configuring the cgroup blockio controller.

**irqbalance_config_file**="/etc/sysconfig/irqbalance"
  Used to change irqbalance service config file which is used by CRI-O.
  For CentOS/SUSE, this file is located at /etc/sysconfig/irqbalance. For Ubuntu, this file is located at /etc/default/irqbalance.

**cgroup_manager**="systemd"
  Cgroup management implementation used for the runtime.

**default_capabilities**=[]
  List of default capabilities for containers. If it is empty or commented out, only the capabilities defined in the container json file by the user/kube will be added.

  The default list is:
```
  default_capabilities = [
	  "CHOWN",
	  "DAC_OVERRIDE",
	  "FSETID",
	  "FOWNER",
	  "SETGID",
	  "SETUID",
	  "SETPCAP",
	  "NET_BIND_SERVICE",
	  "KILL",
  ]
```

**default_sysctls**=[]
 List of default sysctls. If it is empty or commented out, only the sysctls defined in the container json file by the user/kube will be added.

  One example would be allowing ping inside of containers.  On systems that support `/proc/sys/net/ipv4/ping_group_range`, the default list could be:
```
  default_sysctls = [
       "net.ipv4.ping_group_range = 0   2147483647",
  ]
```

**additional_devices**=[]
  List of additional devices. Specified as "<device-on-host>:<device-on-container>:<permissions>", for example: "--additional-devices=/dev/sdc:/dev/xvdc:rwm". If it is empty or commented out, only the devices defined in the container json file by the user/kube will be added.

**hooks_dir**=["*path*", ...]
  Each `*.json` file in the path configures a hook for CRI-O containers.  For more details on the syntax of the JSON files and the semantics of hook injection, see `oci-hooks(5)`.  CRI-O currently support both the 1.0.0 and 0.1.0 hook schemas, although the 0.1.0 schema is deprecated.

  Paths listed later in the array have higher precedence (`oci-hooks(5)` discusses directory precedence).

  For the annotation conditions, CRI-O uses the Kubernetes annotations, which are a subset of the annotations passed to the OCI runtime.  For example, `io.kubernetes.cri-o.Volumes` is part of the OCI runtime configuration annotations, but it is not part of the Kubernetes annotations being matched for hooks.

  For the bind-mount conditions, only mounts explicitly requested by Kubernetes configuration are considered.  Bind mounts that CRI-O inserts by default (e.g. `/dev/shm`) are not considered.

**default_mounts**=[]
  List of default mounts for each container. **Deprecated:** this option will be removed in future versions in favor of `default_mounts_file`.

**default_mounts_file**=""
  Path to the file specifying the defaults mounts for each container. The format of the config is /SRC:/DST, one mount per line. Notice that CRI-O reads its default mounts from the following two files:

    1) `/etc/containers/mounts.conf` (i.e., default_mounts_file): This is the override file, where users can either add in their own default mounts, or override the default mounts shipped with the package.

    2) `/usr/share/containers/mounts.conf`: This is the default file read for mounts. If you want CRI-O to read from a different, specific mounts file, you can change the default_mounts_file. Note, if this is done, CRI-O will only add mounts it finds in this file.

**bind_mount_prefix**="prefix"
  A prefix to use for the source of the bind mounts.

  One potential use for this option would be running CRI-O in a container, and mounting / on the host as /host in the container.  CRI-O could then be ran with --bind-mount-prefix=/host, and CRI-O would add /host to any bind mounts it hands over the CRI, thus specifying the correct directory on the host.

**pids_limit**=1024
  Maximum number of processes allowed in a container.

**log_filter**=""
  Filter the log messages by the provided regular expression. This option supports live configuration reload. For example 'request:.*' filters all gRPC requests.

**log_level**="info"
  Changes the verbosity of the logs based on the level it is set to. Options are fatal, panic, error, warn, info, debug, and trace. This option supports live configuration reload.

**log_size_max**=-1
  Maximum size allowed for the container log file. Negative numbers indicate that no size limit is imposed. If it is positive, it must be >= 8192 to match/exceed conmon's read buffer. The file is truncated and re-opened so the limit is never exceeded.

**log_to_journald**=false
  Whether container output should be logged to journald in addition to the kuberentes log file.

**container_exits_dir**="/var/run/crio/exits"
  Path to directory in which container exit files are written to by conmon.

**container_attach_socket_dir**="/var/run/crio"
  Path to directory for container attach sockets.

**bind_mount_prefix**=""
  A prefix to use for the source of the bind mounts. This option would be useful when running CRI-O in a container and the / directory on the host is mounted as /host in the container. Then if CRI-O runs with the --bind-mount-prefix=/host option, CRI-O would add the /host directory to any bind mounts it hands over to CRI. If Kubernetes asked to have /var/lib/foobar bind mounted into the container, then CRI-O would bind mount /host/var/lib/foobar. Since CRI-O itself is running in a container with / or the host mounted on /host, the container would end up with /var/lib/foobar from the host mounted in the container rather than /var/lib/foobar from the CRI-O container.

**read_only**=false
  If set to true, all containers will run in read-only mode.

**uid_mappings**=""
  The UID mappings for the user namespace of each container. A range is specified in the form containerUID:HostUID:Size. Multiple ranges must be separated by comma.

**gid_mappings**=""
  The GID mappings for the user namespace of each container. A range is specified in the form containerGID:HostGID:Size. Multiple ranges must be separated by comma.

**ctr_stop_timeout**=30
  The minimal amount of time in seconds to wait before issuing a timeout regarding the proper termination of the container.

**manage_ns_lifecycle**=true
  Determines whether we pin and remove namespaces and manage their lifecycle. This option is being deprecated, and will be unconditionally true in the future.

**drop_infra_ctr**=false
  Determines whether we drop the infra container when a pod does not have a private PID namespace, and does not use a kernel separating runtime (like kata).
  Requies **manage_ns_lifecycle** to be true.

**infra_ctr_cpuset**=""
    Determines the CPU set to run infra containers. If not specified, the CRI-O will use all online CPUs to run infra containers.
    You can specify CPUs in the Linux CPU list format.
    To get better isolation for guaranteed pods, set this parameter to be equal to kubelet reserved-cpus.

**namespaces_dir**="/var/run"
  The directory where the state of the managed namespaces gets tracked. Only used when manage_ns_lifecycle is true

**pinns_path**=""
  The path to find the pinns binary, which is needed to manage namespace lifecycle

**absent_mount_sources_to_reject**=[]
  A list of paths that, when absent from the host, will cause a container creation to fail (as opposed to the current behavior of creating a directory).

### CRIO.RUNTIME.RUNTIMES TABLE
The "crio.runtime.runtimes" table defines a list of OCI compatible runtimes.  The runtime to use is picked based on the runtime_handler provided by the CRI.  If no runtime_handler is provided, the runtime will be picked based on the level of trust of the workload.

**runtime_path**=""
  Path to the OCI compatible runtime used for this runtime handler.

**runtime_root**=""
  Root directory used to store runtime data

**runtime_type**="oci"
  Type of the runtime used for this runtime handler. "oci", "vm"

**privileged_without_host_devices**=false
  Whether this runtime handler prevents host devices from being passed to privileged containers.

**allowed_annotations**=[]
  A list of experimental annotations this runtime handler is allowed to process.
  The currently recognized values are:
  "io.kubernetes.cri-o.userns-mode" for configuring a user namespace for the pod.
  "io.kubernetes.cri-o.Devices" for configuring devices for the pod.
  "io.kubernetes.cri-o.ShmSize" for configuring the size of /dev/shm.
  "io.kubernetes.cri-o.UnifiedCgroup.$CTR_NAME" for configuring the cgroup v2 unified block for a container.

## CRIO.IMAGE TABLE
The `crio.image` table contains settings pertaining to the management of OCI images.

CRI-O reads its configured registries defaults from the system wide containers-registries.conf(5) located in /etc/containers/registries.conf. If you want to modify just CRI-O, you can change the registries configuration in this file. Otherwise, leave `insecure_registries` and `registries` commented out to use the system's defaults from /etc/containers/registries.conf.

**default_transport**="docker://"
  Default transport for pulling images from a remote container storage.

**global_auth_file**=""
  The path to a file like /var/lib/kubelet/config.json holding credentials necessary for pulling images from secure registries.

**pause_image**="k8s.gcr.io/pause:3.5"
  The image used to instantiate infra containers. This option supports live configuration reload.

**pause_image_auth_file**=""
 The path to a file like /var/lib/kubelet/config.json holding credentials specific to pulling the pause_image from above. This option supports live configuration reload.

**pause_command**="/pause"
  The command to run to have a container stay in the paused state. This option supports live configuration reload.

**signature_policy**=""
  Path to the file which decides what sort of policy we use when deciding whether or not to trust an image that we've pulled. It is not recommended that this option be used, as the default behavior of using the system-wide default policy (i.e., /etc/containers/policy.json) is most often preferred. Please refer to containers-policy.json(5) for more details.

**image_volumes**="mkdir"
  Controls how image volumes are handled. The valid values are mkdir, bind and ignore; the latter will ignore volumes entirely.

**insecure_registries**=[]
  List of registries to skip TLS verification for pulling images.

**registries**=["docker.io"]
  List of registries to be used when pulling an unqualified image. Note support for this option has been dropped and it has no effect. Please refer to `containers-registries.conf(5)` for configuring unqualified-search registries.

**big_files_temporary_dir**=""
  Path to the temporary directory to use for storing big files, used to store image blobs and data streams related to containers image management.

**separate_pull_cgroup**=""
  [EXPERIMENTAL] If its value is set, then images are pulled into the specified cgroup.  If its value is set to "pod", then the pod's cgroup is used.  It is currently supported only with the systemd cgroup manager.

## CRIO.NETWORK TABLE
The `crio.network` table containers settings pertaining to the management of CNI plugins.

**cni_default_network**=""
  The default CNI network name to be selected. If not set or "", then CRI-O will pick-up the first one found in network_dir.

**network_dir**="/etc/cni/net.d/"
  Path to the directory where CNI configuration files are located.

**plugin_dirs**=["/opt/cni/bin/",]
  List of paths to directories where CNI plugin binaries are located.

## CRIO.METRICS TABLE
The `crio.metrics` table containers settings pertaining to the Prometheus based metrics retrieval.

**enable_metrics**=false
  Globally enable or disable metrics support.

**enable_custom_shm_size**=false
If set to true, enable users to set a custom shm size instead of using the default value of 64M. The shm size can be set through K8S annotation with the key "io.kubernetes.cri-o.ShmSize", and the value representing the size in human readable format. For example: "io.kubernetes.cri-o.ShmSize: 128Mi"

**metrics_port**=9090
  The port on which the metrics server will listen.

**metrics_socket**=""
  The socket on which the metrics server will listen.

**metrics_cert**=""
  The certificate for the secure metrics server.

**metrics_key**=""
  The certificate key for the secure metrics server.

# SEE ALSO
crio.conf.d(5), containers-storage.conf(5), containers-policy.json(5), containers-registries.conf(5), crio(8)

# HISTORY
Aug 2018, Update to the latest state by Valentin Rothberg <vrothberg@suse.com>

Oct 2016, Originally compiled by Aleksa Sarai <asarai@suse.de>

[toml]: https://github.com/toml-lang/toml
[crio]: ./crio.8.md
