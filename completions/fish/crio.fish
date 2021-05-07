# crio fish shell completion

function __fish_crio_no_subcommand --description 'Test if there has been any subcommand yet'
    for i in (commandline -opc)
        if contains -- $i complete completion man markdown md config version wipe help h
            return 1
        end
    end
    return 0
end

complete -c crio -n '__fish_crio_no_subcommand' -f -l absent-mount-sources-to-reject -r -d 'A list of paths that, when absent from the host, will cause a container creation to fail (as opposed to the current behavior of creating a directory).'
complete -c crio -n '__fish_crio_no_subcommand' -f -l additional-devices -r -d 'Devices to add to the containers '
complete -c crio -n '__fish_crio_no_subcommand' -f -l apparmor-profile -r -d 'Name of the apparmor profile to be used as the runtime\'s default. This only takes effect if the user does not specify a profile via the Kubernetes Pod\'s metadata annotation.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l big-files-temporary-dir -r -d 'Path to the temporary directory to use for storing big files, used to store image blobs and data streams related to containers image management.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l bind-mount-prefix -r -d 'A prefix to use for the source of the bind mounts. This option would be useful if you were running CRI-O in a container. And had `/` mounted on `/host` in your container. Then if you ran CRI-O with the `--bind-mount-prefix=/host` option, CRI-O would add /host to any bind mounts it is handed over CRI. If Kubernetes asked to have `/var/lib/foobar` bind mounted into the container, then CRI-O would bind mount `/host/var/lib/foobar`. Since CRI-O itself is running in a container with `/` or the host mounted on `/host`, the container would end up with `/var/lib/foobar` from the host mounted in the container rather then `/var/lib/foobar` from the CRI-O container. (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -f -l blockio-config-file -r -d 'Path to the blockio class configuration file for configuring the cgroup blockio controller.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l cgroup-manager -r -d 'cgroup manager (cgroupfs or systemd)'
complete -c crio -n '__fish_crio_no_subcommand' -l clean-shutdown-file -r -d 'Location for CRI-O to lay down the clean shutdown file. It indicates whether we\'ve had time to sync changes to disk before shutting down. If not found, crio wipe will clear the storage directory'
complete -c crio -n '__fish_crio_no_subcommand' -l cni-config-dir -r -d 'CNI configuration files directory'
complete -c crio -n '__fish_crio_no_subcommand' -f -l cni-default-network -r -d 'Name of the default CNI network to select. If not set or "", then CRI-O will pick-up the first one found in --cni-config-dir.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l cni-plugin-dir -r -d 'CNI plugin binaries directory'
complete -c crio -n '__fish_crio_no_subcommand' -l config -s c -r -d 'Path to configuration file'
complete -c crio -n '__fish_crio_no_subcommand' -l config-dir -s d -r -d 'Path to the configuration drop-in directory.
    This directory will be recursively iterated and each file gets applied
    to the configuration in their processing order. This means that a
    configuration file named \'00-default\' has a lower priority than a file
    named \'01-my-overwrite\'.
    The global config file, provided via \'--config,-c\' or per default in
    /etc/crio/crio.conf, always has a lower priority than the files in the directory specified
    by \'--config-dir,-d\'.
    Besides that, provided command line parameters have a higher priority
    than any configuration file.'
complete -c crio -n '__fish_crio_no_subcommand' -l conmon -r -d 'Path to the conmon binary, used for monitoring the OCI runtime. Will be searched for using $PATH if empty. (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -f -l conmon-cgroup -r -d 'cgroup to be used for conmon process'
complete -c crio -n '__fish_crio_no_subcommand' -f -l conmon-env -r -d 'Environment variable list for the conmon process, used for passing necessary environment variables to conmon or the runtime'
complete -c crio -n '__fish_crio_no_subcommand' -l container-attach-socket-dir -r -d 'Path to directory for container attach sockets'
complete -c crio -n '__fish_crio_no_subcommand' -l container-exits-dir -r -d 'Path to directory in which container exit files are written to by conmon'
complete -c crio -n '__fish_crio_no_subcommand' -f -l ctr-stop-timeout -r -d 'The minimal amount of time in seconds to wait before issuing a timeout regarding the proper termination of the container. The lowest possible value is 30s, whereas lower values are not considered by CRI-O'
complete -c crio -n '__fish_crio_no_subcommand' -f -l decryption-keys-path -r -d 'Path to load keys for image decryption.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l default-capabilities -r -d 'Capabilities to add to the containers'
complete -c crio -n '__fish_crio_no_subcommand' -f -l default-env -r -d 'Additional environment variables to set for all containers'
complete -c crio -n '__fish_crio_no_subcommand' -l default-mounts-file -r -d 'Path to default mounts file (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -f -l default-runtime -r -d 'Default OCI runtime from the runtimes config'
complete -c crio -n '__fish_crio_no_subcommand' -f -l default-sysctls -r -d 'Sysctls to add to the containers'
complete -c crio -n '__fish_crio_no_subcommand' -f -l default-transport -r -d 'A prefix to prepend to image names that cannot be pulled as-is'
complete -c crio -n '__fish_crio_no_subcommand' -f -l default-ulimits -r -d 'Ulimits to apply to containers by default (name=soft:hard) (default: [])'
complete -c crio -n '__fish_crio_no_subcommand' -f -l drop-infra-ctr -d 'Determines whether pods are created without an infra container, when the pod is not using a pod level PID namespace (default: false)'
complete -c crio -n '__fish_crio_no_subcommand' -f -l enable-metrics -d 'Enable metrics endpoint for the server on localhost:9090'
complete -c crio -n '__fish_crio_no_subcommand' -f -l enable-profile-unix-socket -d 'Enable pprof profiler on crio unix domain socket'
complete -c crio -n '__fish_crio_no_subcommand' -f -l gid-mappings -r -d 'Specify the GID mappings to use for the user namespace (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -l global-auth-file -r -d 'Path to a file like /var/lib/kubelet/config.json holding credentials necessary for pulling images from secure registries (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -f -l grpc-max-recv-msg-size -r -d 'Maximum grpc receive message size in bytes'
complete -c crio -n '__fish_crio_no_subcommand' -f -l grpc-max-send-msg-size -r -d 'Maximum grpc receive message size'
complete -c crio -n '__fish_crio_no_subcommand' -f -l hooks-dir -r -d 'Set the OCI hooks directory path (may be set multiple times)
    If one of the directories does not exist, then CRI-O will automatically
    skip them.
    Each \'\*.json\' file in the path configures a hook for CRI-O
    containers. For more details on the syntax of the JSON files and
    the semantics of hook injection, see \'oci-hooks(5)\'. CRI-O
    currently support both the 1.0.0 and 0.1.0 hook schemas, although
    the 0.1.0 schema is deprecated.
    This option may be set multiple times; paths from later options
    have higher precedence (\'oci-hooks(5)\' discusses directory
    precedence).
    For the annotation conditions, CRI-O uses the Kubernetes
    annotations, which are a subset of the annotations passed to the
    OCI runtime. For example, \'io.kubernetes.cri-o.Volumes\' is part of
    the OCI runtime configuration annotations, but it is not part of
    the Kubernetes annotations being matched for hooks.
    For the bind-mount conditions, only mounts explicitly requested by
    Kubernetes configuration are considered. Bind mounts that CRI-O
    inserts by default (e.g. \'/dev/shm\') are not considered.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l image-volumes -r -d 'Image volume handling (\'mkdir\', \'bind\', or \'ignore\')
    1. mkdir: A directory is created inside the container root filesystem for
       the volumes.
    2. bind: A directory is created inside container state directory and bind
       mounted into the container for the volumes.
	3. ignore: All volumes are just ignored and no action is taken.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l infra-ctr-cpuset -r -d 'CPU set to run infra containers, if not specified CRI-O will use all online CPUs to run infra containers (default: \'\').'
complete -c crio -n '__fish_crio_no_subcommand' -f -l insecure-registry -r -d 'Enable insecure registry communication, i.e., enable un-encrypted and/or untrusted communication.
    1. List of insecure registries can contain an element with CIDR notation to
       specify a whole subnet.
    2. Insecure registries accept HTTP or accept HTTPS with certificates from
       unknown CAs.
    3. Enabling \'--insecure-registry\' is useful when running a local registry.
       However, because its use creates security vulnerabilities, **it should ONLY
       be enabled for testing purposes**. For increased security, users should add
       their CA to their system\'s list of trusted CAs instead of using
       \'--insecure-registry\'.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l internal-wipe -d 'Whether CRI-O should wipe containers after a reboot and images after an upgrade when the server starts. If set to false, one must run `crio wipe` to wipe the containers and images in these situations.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l irqbalance-config-file -r -d 'The irqbalance service config file which is used by CRI-O.'
complete -c crio -n '__fish_crio_no_subcommand' -l listen -r -d 'Path to the CRI-O socket'
complete -c crio -n '__fish_crio_no_subcommand' -l log -r -d 'Set the log file path where internal debug information is written'
complete -c crio -n '__fish_crio_no_subcommand' -l log-dir -r -d 'Default log directory where all logs will go unless directly specified by the kubelet'
complete -c crio -n '__fish_crio_no_subcommand' -f -l log-filter -r -d 'Filter the log messages by the provided regular expression. For example \'request.\*\' filters all gRPC requests.'
complete -c crio -n '__fish_crio_no_subcommand' -f -l log-format -r -d 'Set the format used by logs: \'text\' or \'json\''
complete -c crio -n '__fish_crio_no_subcommand' -f -l log-journald -d 'Log to systemd journal (journald) in addition to kubernetes log file (default: false)'
complete -c crio -n '__fish_crio_no_subcommand' -f -l log-level -s l -r -d 'Log messages above specified level: trace, debug, info, warn, error, fatal or panic'
complete -c crio -n '__fish_crio_no_subcommand' -f -l log-size-max -r -d 'Maximum log size in bytes for a container. If it is positive, it must be >= 8192 to match/exceed conmon read buffer'
complete -c crio -n '__fish_crio_no_subcommand' -f -l metrics-cert -r -d 'Certificate for the secure metrics endpoint'
complete -c crio -n '__fish_crio_no_subcommand' -f -l metrics-key -r -d 'Certificate key for the secure metrics endpoint'
complete -c crio -n '__fish_crio_no_subcommand' -f -l metrics-port -r -d 'Port for the metrics endpoint'
complete -c crio -n '__fish_crio_no_subcommand' -f -l metrics-socket -r -d 'Socket for the metrics endpoint'
complete -c crio -n '__fish_crio_no_subcommand' -f -l namespaces-dir -r -d 'The directory where the state of the managed namespaces gets tracked. Only used when manage-ns-lifecycle is true'
complete -c crio -n '__fish_crio_no_subcommand' -f -l no-pivot -d 'If true, the runtime will not use `pivot_root`, but instead use `MS_MOVE` (default: false)'
complete -c crio -n '__fish_crio_no_subcommand' -f -l pause-command -r -d 'Path to the pause executable in the pause image'
complete -c crio -n '__fish_crio_no_subcommand' -f -l pause-image -r -d 'Image which contains the pause executable'
complete -c crio -n '__fish_crio_no_subcommand' -l pause-image-auth-file -r -d 'Path to a config file containing credentials for --pause-image (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -f -l pids-limit -r -d 'Maximum number of processes allowed in a container'
complete -c crio -n '__fish_crio_no_subcommand' -f -l pinns-path -r -d 'The path to find the pinns binary, which is needed to manage namespace lifecycle. Will be searched for in $PATH if empty (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -f -l profile -d 'Enable pprof remote profiler on localhost:6060'
complete -c crio -n '__fish_crio_no_subcommand' -f -l profile-port -r -d 'Port for the pprof profiler'
complete -c crio -n '__fish_crio_no_subcommand' -f -l read-only -d 'Setup all unprivileged containers to run as read-only. Automatically mounts tmpfs on `/run`, `/tmp` and `/var/tmp`. (default: false)'
complete -c crio -n '__fish_crio_no_subcommand' -f -l registry -r -d 'Registry to be prepended when pulling unqualified images, can be specified multiple times'
complete -c crio -n '__fish_crio_no_subcommand' -l root -s r -r -d 'The CRI-O root directory'
complete -c crio -n '__fish_crio_no_subcommand' -l runroot -r -d 'The CRI-O state directory'
complete -c crio -n '__fish_crio_no_subcommand' -f -l runtimes -r -d 'OCI runtimes, format is runtime_name:runtime_path:runtime_root:runtime_type'
complete -c crio -n '__fish_crio_no_subcommand' -l seccomp-profile -r -d 'Path to the seccomp.json profile to be used as the runtime\'s default. If not specified, then the internal default seccomp profile will be used. (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -f -l seccomp-use-default-when-empty -r -d 'Use the default seccomp profile when an empty one is specified (default: false)'
complete -c crio -n '__fish_crio_no_subcommand' -f -l selinux -d 'Enable selinux support (default: false)'
complete -c crio -n '__fish_crio_no_subcommand' -f -l separate-pull-cgroup -r -d '[EXPERIMENTAL] Pull in new cgroup (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -l signature-policy -r -d 'Path to signature policy JSON file. (default: "", to use the system-wide default)'
complete -c crio -n '__fish_crio_no_subcommand' -f -l storage-driver -s s -r -d 'OCI storage driver (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -f -l storage-opt -r -d 'OCI storage driver option'
complete -c crio -n '__fish_crio_no_subcommand' -f -l stream-address -r -d 'Bind address for streaming socket'
complete -c crio -n '__fish_crio_no_subcommand' -f -l stream-enable-tls -d 'Enable encrypted TLS transport of the stream server (default: false)'
complete -c crio -n '__fish_crio_no_subcommand' -f -l stream-idle-timeout -r -d 'Length of time until open streams terminate due to lack of activity'
complete -c crio -n '__fish_crio_no_subcommand' -f -l stream-port -r -d 'Bind port for streaming socket. If the port is set to \'0\', then CRI-O will allocate a random free port number.'
complete -c crio -n '__fish_crio_no_subcommand' -l stream-tls-ca -r -d 'Path to the x509 CA(s) file used to verify and authenticate client communication with the encrypted stream. This file can change and CRI-O will automatically pick up the changes within 5 minutes (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -l stream-tls-cert -r -d 'Path to the x509 certificate file used to serve the encrypted stream. This file can change and CRI-O will automatically pick up the changes within 5 minutes (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -l stream-tls-key -r -d 'Path to the key file used to serve the encrypted stream. This file can change and CRI-O will automatically pick up the changes within 5 minutes (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -f -l uid-mappings -r -d 'Specify the UID mappings to use for the user namespace (default: "")'
complete -c crio -n '__fish_crio_no_subcommand' -l version-file -r -d 'Location for CRI-O to lay down the temporary version file. It is used to check if crio wipe should wipe containers, which should always happen on a node reboot'
complete -c crio -n '__fish_crio_no_subcommand' -l version-file-persist -r -d 'Location for CRI-O to lay down the persistent version file. It is used to check if crio wipe should wipe images, which should only happen when CRI-O has been upgraded'
complete -c crio -n '__fish_crio_no_subcommand' -f -l help -s h -d 'show help'
complete -c crio -n '__fish_crio_no_subcommand' -f -l version -s v -d 'print the version'
complete -c crio -n '__fish_crio_no_subcommand' -f -l help -s h -d 'show help'
complete -c crio -n '__fish_crio_no_subcommand' -f -l version -s v -d 'print the version'
complete -c crio -n '__fish_seen_subcommand_from complete completion' -f -l help -s h -d 'show help'
complete -r -c crio -n '__fish_crio_no_subcommand' -a 'complete completion' -d 'Generate bash, fish or zsh completions.'
complete -c crio -n '__fish_seen_subcommand_from complete completion' -f -l help -s h -d 'show help'
complete -c crio -n '__fish_seen_subcommand_from man' -f -l help -s h -d 'show help'
complete -r -c crio -n '__fish_crio_no_subcommand' -a 'man' -d 'Generate the man page documentation.'
complete -c crio -n '__fish_seen_subcommand_from markdown md' -f -l help -s h -d 'show help'
complete -r -c crio -n '__fish_crio_no_subcommand' -a 'markdown md' -d 'Generate the markdown documentation.'
complete -c crio -n '__fish_seen_subcommand_from config' -f -l help -s h -d 'show help'
complete -r -c crio -n '__fish_crio_no_subcommand' -a 'config' -d 'Outputs a commented version of the configuration file that could be used
by CRI-O. This allows you to save you current configuration setup and then load
it later with **--config**. Global options will modify the output.'
complete -c crio -n '__fish_seen_subcommand_from config' -f -l default -d 'Output the default configuration (without taking into account any configuration options).'
complete -c crio -n '__fish_seen_subcommand_from config' -f -l migrate-defaults -s m -r -d 'Migrate the default config from a specified version.
    To run a config migration, just select the input config via the global
    \'--config,-c\' command line argument, for example:
    ```
    crio -c /etc/crio/crio.conf.d/00-default.conf config -m 1.17
    ```
    The migration will print converted configuration options to stderr and will
    output the resulting configuration to stdout.
    Please note that the migration will overwrite any fields that have changed
    defaults between versions. To save a custom configuration change, it should
    be in a drop-in configuration file instead.
    Possible values: "1.17"'
complete -c crio -n '__fish_seen_subcommand_from version' -f -l help -s h -d 'show help'
complete -r -c crio -n '__fish_crio_no_subcommand' -a 'version' -d 'display detailed version information'
complete -c crio -n '__fish_seen_subcommand_from version' -f -l json -s j -d 'print JSON instead of text'
complete -c crio -n '__fish_seen_subcommand_from wipe' -f -l help -s h -d 'show help'
complete -r -c crio -n '__fish_crio_no_subcommand' -a 'wipe' -d 'wipe CRI-O\'s container and image storage'
complete -c crio -n '__fish_seen_subcommand_from wipe' -f -l force -s f -d 'force wipe by skipping the version check'
complete -c crio -n '__fish_seen_subcommand_from help h' -f -l help -s h -d 'show help'
complete -r -c crio -n '__fish_crio_no_subcommand' -a 'help h' -d 'Shows a list of commands or help for one command'
