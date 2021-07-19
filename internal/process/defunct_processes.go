package process

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefunctProcesses returns the PIDs of all the zombie processes in the node
func DefunctProcesses() ([]int, error) {
	directories, err := os.Open("/proc")
	if err != nil {
		return nil, err
	}
	defer directories.Close()

	names, err := directories.Readdirnames(-1)
	if err != nil {
		return nil, err
	}

	var pids []int
	for _, name := range names {
		// processes have numeric names. If the name does not start with a number, skip.
		if name[0] < '0' || name[0] > '9' {
			continue
		}

		pid, err := strconv.ParseInt(name, 10, 0)
		if err != nil {
			continue
		}

		stat, err := status(int(pid))
		if err != nil {
			continue
		}
		if stat == "Z" {
			pids = append(pids, int(pid))
		}
	}
	return pids, nil
}

// status returns the status of a process
func status(pid int) (string, error) {
	bytes, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	data := string(bytes)

	i := strings.LastIndexByte(data, ')')
	if i <= 2 || i >= len(data)-1 {
		return "", fmt.Errorf("invalid stat data (no comm): %q", data)
	}
	return string(data[i+2]), nil
}
