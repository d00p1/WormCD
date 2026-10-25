package manifest

import (
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

func checkProject(project *types.Project) error {
	var issues []string
	if len(project.Jobs) > 0 {
		issues = append(issues, "jobs are not supported")
	}
	if len(project.Models) > 0 {
		issues = append(issues, "models are not supported")
	}
	if len(project.DisabledServices) > 0 {
		issues = append(issues, fmt.Sprintf(
			"profiles are not supported (disabled services: %s)",
			strings.Join(sortedKeys(project.DisabledServices), ", "),
		))
	}
	if len(issues) > 0 {
		return fmt.Errorf("%s", strings.Join(issues, "; "))
	}
	return nil
}

// unsupportedServiceFields lists Compose fields WormCD does not map onto the
// normalized model yet. Silently ignoring them would make a plan claim things
// that will never be applied, so they are rejected explicitly.
func unsupportedServiceFields(s types.ServiceConfig) []string {
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}

	add("annotations", len(s.Annotations) > 0)
	add("attach", s.Attach != nil)
	add("blkio_config", s.BlkioConfig != nil)
	add("build", s.Build != nil)
	add("cap_add", len(s.CapAdd) > 0)
	add("cap_drop", len(s.CapDrop) > 0)
	add("cgroup", s.Cgroup != "")
	add("cgroup_parent", s.CgroupParent != "")
	add("container_name", s.ContainerName != "")
	add("cpu_count", s.CPUCount != 0)
	add("cpu_percent", s.CPUPercent != 0)
	add("cpu_period", s.CPUPeriod != 0)
	add("cpu_quota", s.CPUQuota != 0)
	add("cpu_rt_period", s.CPURTPeriod != 0)
	add("cpu_rt_runtime", s.CPURTRuntime != 0)
	add("cpus", s.CPUS != 0)
	add("cpuset", s.CPUSet != "")
	add("cpu_shares", s.CPUShares != 0)
	add("credential_spec", s.CredentialSpec != nil)
	add("depends_on", len(s.DependsOn) > 0)
	add("develop", s.Develop != nil)
	add("device_cgroup_rules", len(s.DeviceCgroupRules) > 0)
	add("devices", len(s.Devices) > 0)
	add("dns", len(s.DNS) > 0)
	add("dns_opt", len(s.DNSOpts) > 0)
	add("dns_search", len(s.DNSSearch) > 0)
	add("dockerfile", s.Dockerfile != "")
	add("domainname", s.DomainName != "")
	add("env_file", len(s.EnvFiles) > 0)
	add("expose", len(s.Expose) > 0)
	add("extends", s.Extends != nil)
	add("external_links", len(s.ExternalLinks) > 0)
	add("extra_hosts", len(s.ExtraHosts) > 0)
	add("gpus", len(s.Gpus) > 0)
	add("group_add", len(s.GroupAdd) > 0)
	add("init", s.Init != nil)
	add("ipc", s.Ipc != "")
	add("isolation", s.Isolation != "")
	add("label_file", len(s.LabelFiles) > 0)
	add("links", len(s.Links) > 0)
	add("logging", s.Logging != nil || s.LogDriver != "" || len(s.LogOpt) > 0)
	add("mac_address", s.MacAddress != "")
	add("mem_limit", s.MemLimit != 0)
	add("mem_reservation", s.MemReservation != 0)
	add("mem_swap_limit", s.MemSwapLimit != 0)
	add("mem_swappiness", s.MemSwappiness != 0)
	add("models", len(s.Models) > 0)
	add("net", s.Net != "")
	add("network_mode", s.NetworkMode != "")
	add("oom_kill_disable", s.OomKillDisable)
	add("oom_score_adj", s.OomScoreAdj != 0)
	add("pid", s.Pid != "")
	add("pids_limit", s.PidsLimit != 0)
	add("platform", s.Platform != "")
	add("post_start", len(s.PostStart) > 0)
	add("pre_start", len(s.PreStart) > 0)
	add("pre_stop", len(s.PreStop) > 0)
	add("privileged", s.Privileged)
	add("profiles", len(s.Profiles) > 0)
	add("provider", s.Provider != nil)
	add("pull_policy", s.PullPolicy != "")
	add("pull_refresh_after", s.PullRefreshAfter != "")
	add("restart", s.Restart != "")
	add("runtime", s.Runtime != "")
	add("security_opt", len(s.SecurityOpt) > 0)
	add("shm_size", s.ShmSize != 0)
	add("stdin_open", s.StdinOpen)
	add("storage_opt", len(s.StorageOpt) > 0)
	add("sysctls", len(s.Sysctls) > 0)
	add("tmpfs", len(s.Tmpfs) > 0)
	add("tty", s.Tty)
	add("ulimits", len(s.Ulimits) > 0)
	add("umask", s.Umask != nil)
	add("use_api_socket", s.UseAPISocket)
	add("userns_mode", s.UserNSMode != "")
	add("uts", s.Uts != "")
	add("volume_driver", s.VolumeDriver != "")
	add("volumes_from", len(s.VolumesFrom) > 0)

	return out
}
