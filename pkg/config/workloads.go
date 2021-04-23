package config

import (
	"strconv"

	"github.com/cri-o/cri-o/internal/config/cgmgr"
	cgcfgs "github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	libresource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

const (
	CPUShareResource = "cpushares"
	CPUSetResource   = "cpuset"
)

type Workloads map[string]*WorkloadConfig

type WorkloadConfig struct {
	// ActivationAnnotation is the pod annotation that activates these workload settings
	ActivationAnnotation string `toml:"activation_annotation"`
	// AnnotationPrefix is the way a pod can override a specific resource for a container.
	// The full annotation must be of the form $annotation_prefix.$resource/$ctrname = $value
	AnnotationPrefix string `toml:"annotation_prefix"`
	// Resources are the names of the resources that can be overridden by annotation.
	// The key of the map is the resource name. The following resources are supported:
	// `cpushares`: configure cpu shares for a given container
	// `cpuset`: configure cpuset for a given container
	// The value of the map is the default value for that resource.
	// If a container is configured to use this workload, and does not specify
	// the annotation with the resource and value, the default value will apply.
	// Default values do not need to be specified.
	Resources map[string]string `toml:"resources"`
}

func (w Workloads) Validate() error {
	for workload, config := range w {
		if err := config.Validate(workload); err != nil {
			return err
		}
	}
	return nil
}

func (w *WorkloadConfig) Validate(workloadName string) error {
	if w.ActivationAnnotation == "" {
		return errors.Errorf("annotation shouldn't be empty for workload %q", workloadName)
	}
	for resource, defaultValue := range w.Resources {
		m, ok := mutators[resource]
		if !ok {
			return errors.Errorf("process resource %s for workload %s: resource not supported", resource, workloadName)
		}
		if err := m.ValidateDefault(defaultValue); err != nil {
			return errors.Wrapf(err, "process resource %s for workload %s: default value %s invalid", resource, workloadName, defaultValue)
		}
	}
	return nil
}

func (w Workloads) MutateSpecGivenAnnotations(ctrName string, specgen *generate.Generator, sboxAnnotations map[string]string) error {
	workload := w.workloadGivenActivationAnnotation(sboxAnnotations)
	if workload == nil {
		return nil
	}
	for resource, defaultValue := range workload.Resources {
		value := valueFromAnnotation(resource, defaultValue, workload.AnnotationPrefix, ctrName, sboxAnnotations)
		if value == "" {
			continue
		}

		m, ok := mutators[resource]
		if !ok {
			// CRI-O bug
			panic(errors.Errorf("resource %s is not defined", resource))
		}

		if err := m.MutateSpec(specgen, value); err != nil {
			return errors.Wrapf(err, "mutating spec given workload %s", workload.ActivationAnnotation)
		}
	}
	return nil
}

func (w Workloads) MutateCgroupGivenAnnotations(mgr cgmgr.CgroupManager, sbParent string, sboxAnnotations map[string]string) error {
	workload := w.workloadGivenActivationAnnotation(sboxAnnotations)
	if workload == nil {
		return nil
	}
	cgroup := &cgcfgs.Cgroup{
		Name:      sbParent,
		Resources: &cgcfgs.Resources{},
	}
	for resource, defaultValue := range workload.Resources {
		value := valueFromAnnotation(resource, defaultValue, workload.AnnotationPrefix, "POD", sboxAnnotations)
		if value == "" {
			continue
		}

		m, ok := mutators[resource]
		if !ok {
			// CRI-O bug
			panic(errors.Errorf("resource %s is not defined", resource))
		}

		if err := m.MutateCgroup(cgroup, value); err != nil {
			return errors.Wrapf(err, "mutating spec given workload %s", workload.ActivationAnnotation)
		}
	}
	if err := mgr.Apply(sbParent, cgroup); err != nil {
		return err
	}
	return nil
}

func (w Workloads) workloadGivenActivationAnnotation(sboxAnnotations map[string]string) *WorkloadConfig {
	for _, wc := range w {
		for annotation := range sboxAnnotations {
			if wc.ActivationAnnotation == annotation {
				return wc
			}
		}
	}
	return nil
}

func valueFromAnnotation(resource, defaultValue, prefix, ctrName string, annotations map[string]string) string {
	annotationKey := prefix + "." + resource + "/" + ctrName
	value, ok := annotations[annotationKey]
	if !ok {
		return defaultValue
	}
	return value
}

var mutators = map[string]Mutator{
	CPUShareResource: new(cpuShareMutator),
	CPUSetResource:   new(cpusetMutator),
}

type Mutator interface {
	ValidateDefault(string) error
	MutateSpec(*generate.Generator, string) error
	MutateCgroup(*cgcfgs.Cgroup, string) error
}

type cpusetMutator struct{}

func (m *cpusetMutator) ValidateDefault(set string) error {
	if set == "" {
		return nil
	}
	_, err := cpuset.Parse(set)
	return err
}

func (*cpusetMutator) MutateSpec(specgen *generate.Generator, configuredValue string) error {
	specgen.SetLinuxResourcesCPUCpus(configuredValue)
	return nil
}

func (*cpusetMutator) MutateCgroup(cgroup *cgcfgs.Cgroup, configuredValue string) error {
	if cgroup == nil {
		cgroup = &cgcfgs.Cgroup{}
	}
	if cgroup.Resources == nil {
		cgroup.Resources = &cgcfgs.Resources{}
	}
	cgroup.Resources.CpusetCpus = configuredValue

	return nil
}

type cpuShareMutator struct{}

func (*cpuShareMutator) ValidateDefault(cpuShare string) error {
	if cpuShare == "" {
		return nil
	}
	if _, err := libresource.ParseQuantity(cpuShare); err != nil {
		return err
	}
	return nil
}

func (*cpuShareMutator) MutateSpec(specgen *generate.Generator, configuredValue string) error {
	u, err := strconv.ParseUint(configuredValue, 0, 64)
	if err != nil {
		return err
	}
	specgen.SetLinuxResourcesCPUShares(u)
	return nil
}

func (*cpuShareMutator) MutateCgroup(cgroup *cgcfgs.Cgroup, configuredValue string) error {
	u, err := strconv.ParseUint(configuredValue, 0, 64)
	if err != nil {
		return err
	}

	if cgroup == nil {
		cgroup = &cgcfgs.Cgroup{}
	}
	if cgroup.Resources == nil {
		cgroup.Resources = &cgcfgs.Resources{}
	}

	cgroup.Resources.CpuShares = u
	return nil
}
