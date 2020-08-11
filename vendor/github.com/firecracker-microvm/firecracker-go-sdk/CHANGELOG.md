# 0.20.0
* Moves the NetNS field to `Config` from `JailerConfig` (#155).
* Supports forcing CNI network creation (#130).
* Adds `FIRECRACKER_GO_SDK_INIT_TIMEOUT_SECONDS` and `FIRECRACKER_GO_SDK_REQUEST_TIMEOUT_MILLISECONDS` environment variables to configure timeouts (#165).
* Adds `ForwardSignals` to explicitly configure signal handling (#166).

# 0.19.0
* Firecracker v0.19 API: Vsock API call: PUT /vsocks/{id} changed to PUT /vsock and no longer
  appear to support multiple vsock devices. Any subsequent calls to this API
  endpoint will override the previous vsock device configuration.
* Firecracker v0.19 API: Removed 'Halting' and 'Halted' instance states.

# 0.18.0
* Adds support for configuring Network Interfaces via CNI (#126)
* Moves NetworkInterface.HostDevName and NetworkInterface.MacAddress fields to
  NetworkInterface.StaticConfiguration.HostDevName and NetworkInterface.StaticConfiguration.MacAddress
  fields, respectively. This is a backwards incompatible change, users will need
  to update the location of these fields. (#126)

# 0.17.0

* Fixes a bug where fifos were not working properly with jailer enabled (#96)
* Fixes bug where context was not being used at all during startVM (#86)
* Updates the jailer's socket path to point to the unix socket in the jailer's workspace (#86)
* Fixes bug where default socketpath would always be used when not using jailer (#84).
* Update for compatibility with Firecracker 0.17.x
* Changes JailerCfg to be a pointer and removes EnableJailer for ease of use (#110).

# 0.15.1

* Add the machine.Shutdown() method, enabling access to the SendCtrlAltDel API
  added in Firecracker 0.15.0

# 0.15.0

* Initial release
