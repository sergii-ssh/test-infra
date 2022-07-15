/*
Copyright 2019 Istio Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	dockername "github.com/google/go-containerregistry/pkg/name"
	flag "github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	prowjob "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"sigs.k8s.io/yaml"

	"istio.io/test-infra/tools/prowtrans/pkg/configuration"
	"istio.io/test-infra/tools/prowtrans/pkg/util"
)

const (
	autogenHeader     = "# THIS FILE IS AUTOGENERATED. DO NOT EDIT. See tools/prowtrans/README.md\n"
	filenameSeparator = "."
	jobnameSeparator  = "_"
	gitHost           = "github.com"
	maxLabelLen       = 63
	defaultModifier   = "private"
	defaultCluster    = "default"
	defaultsFilename  = ".defaults.yaml"
	yamlExt           = ".(yml|yaml)$"
	gerritReportLabel = "prow.k8s.io/gerrit-report-label"
)

var defaultJobTypes = []string{"presubmit", "postsubmit", "periodic"}

// sortOrder is the type to define sort order.
type sortOrder string

const (
	ascending  sortOrder = "asc"
	descending sortOrder = "desc"
)

// options are the available command-line flags.
type options struct {
	Configs           []string
	Global            string
	EnvDenylistSet    sets.String
	VolumeDenylistSet sets.String
	JobAllowlistSet   sets.String
	JobDenylistSet    sets.String
	RepoAllowlistSet  sets.String
	RepoDenylistSet   sets.String
	JobTypeSet        sets.String
	configuration.Transform
}

// parseOpts parses the command-line flags.
func (o *options) parseOpts() {
	flag.StringVar(&o.Bucket, "bucket", "", "GCS bucket name to upload logs and build artifacts to.")
	flag.StringVar(&o.Cluster, "cluster", "", "GCP cluster to run the job(s) in.")
	flag.StringVar(&o.Channel, "channel", "", "Slack channel to report job status notifications to.")
	flag.StringVar(&o.Global, "global", "", "Path to file containing global defaults configuration.")
	flag.StringVar(&o.SSHKeySecret, "ssh-key-secret", "", "GKE cluster secrets containing the Github ssh private key.")
	flag.StringVar(&o.Modifier, "modifier", defaultModifier, "Modifier to apply to generated file and job name(s).")
	flag.StringVar(&o.ServiceAccount, "service-account", "", "Service Account to apply to generated files.")
	flag.StringVar(&o.Tag, "tag", "", "Override docker image tag for generated job(s).")
	flag.StringVarP(&o.Input, "input", "i", ".", "Input file or directory containing job(s) to convert.")
	flag.StringVarP(&o.Output, "output", "o", ".", "Output file or directory to write generated job(s).")
	flag.StringVarP(&o.Sort, "sort", "s", "", "Sort the job(s) by name: (e.g. (asc)ending, (desc)ending).")
	flag.StringSliceVar(&o.Branches, "branches", []string{}, "Branch(es) to generate job(s) for.")
	flag.StringSliceVar(&o.BranchesOut, "branches-out", []string{}, "Override output branch(es) for generated presubmit and postsubmit job(s).")
	flag.StringVar(&o.RefBranchOut, "ref-branch-out", "", "Override ref branch for generated periodic job(s).")
	flag.StringSliceVar(&o.Configs, "configs", []string{}, "Path to files or directories containing yaml job transforms.")
	flag.StringSliceVarP(&o.Presets, "presets", "p", []string{}, "Path to file(s) containing additional presets.")
	flag.StringSliceVar(&o.RerunOrgs, "rerun-orgs", []string{}, "GitHub organizations to authorize job rerun for.")
	flag.StringSliceVar(&o.RerunUsers, "rerun-users", []string{}, "GitHub user to authorize job rerun for.")
	flag.StringToStringVar(&o.Selector, "selector", map[string]string{}, "Node selector(s) to constrain job(s).")
	flag.StringToStringVarP(&o.Labels, "labels", "l", map[string]string{}, "Prow labels to apply to the job(s).")
	flag.StringToStringVarP(&o.Env, "env", "e", map[string]string{}, "Environment variables to set for the job(s).")
	flag.StringToStringVarP(&o.OrgMap, "mapping", "m", map[string]string{}, "Mapping between public and private Github organization(s).")
	flag.StringToStringVar(&o.RefOrgMap, "ref-mapping", map[string]string{}, "Mapping between public and private Github organization(s) in refs.")
	flag.StringToStringVar(&o.HubMap, "hub-mapping", map[string]string{}, "Docker image hub mapping.")
	flag.StringToStringVarP(&o.Annotations, "annotations", "a", map[string]string{}, "Annotations to apply to the job(s)")
	flag.StringSliceVar(&o.EnvDenylist, "env-denylist", []string{}, "Env(s) to denylist in generation process.")
	flag.StringSliceVar(&o.VolumeDenylist, "volume-denylist", []string{}, "Volume(s) to denylist in generation process.")
	flag.StringSliceVar(&o.JobAllowlist, "job-allowlist", []string{}, "Job(s) to allowlist in generation process.")
	flag.StringSliceVar(&o.JobDenylist, "job-denylist", []string{}, "Job(s) to denylist in generation process.")
	flag.StringSliceVar(&o.RepoAllowlist, "repo-allowlist", []string{}, "Repositories to allowlist in generation process.")
	flag.StringSliceVar(&o.RepoDenylist, "repo-denylist", []string{}, "Repositories to denylist in generation process.")
	flag.StringSliceVarP(&o.JobType, "job-type", "t", defaultJobTypes, "Job type(s) to process (e.g. presubmit, postsubmit. periodic).")
	flag.BoolVar(&o.Clean, "clean", false, "Clean output files before job(s) generation.")
	flag.BoolVar(&o.DryRun, "dry-run", false, "Run in dry run mode.")
	flag.BoolVar(&o.Refs, "refs", false, "Apply translation to all extra refs regardless of repo.")
	flag.BoolVar(&o.Resolve, "resolve", false, "Resolve and expand values for presets in generated job(s).")
	flag.BoolVar(&o.SSHClone, "ssh-clone", false, "Enable a clone of the git repository over ssh.")
	flag.BoolVar(&o.OverrideSelector, "override-selector", false, "The existing node selector will be overridden rather than added to.")
	flag.BoolVar(&o.SupportGerritReporting, "support-gerrit-reporting", false, "Generate Prow jobs that supports Gerrit reporting.")
	flag.BoolVar(&o.AllowLongJobNames, "allow-long-job-names", false, "Allow job names that have more than 63 characters.")
	flag.BoolVar(&o.Verbose, "verbose", false, "Enable verbose output.")

	flag.Parse()

	o.EnvDenylistSet = sets.NewString(o.EnvDenylist...)
	o.VolumeDenylistSet = sets.NewString(o.VolumeDenylist...)
	o.JobAllowlistSet = sets.NewString(o.JobAllowlist...)
	o.JobDenylistSet = sets.NewString(o.JobDenylist...)
	o.RepoAllowlistSet = sets.NewString(o.RepoAllowlist...)
	o.RepoDenylistSet = sets.NewString(o.RepoDenylist...)
	o.JobTypeSet = sets.NewString(o.JobType...)
}

// parseConfiguration parses the yaml configuration transforms.
func (o *options) parseConfiguration() []options {
	var optsList []options
	var global configuration.Configuration

	if o.Global != "" {
		if d, err := ioutil.ReadFile(o.Global); err == nil {
			if err := yaml.UnmarshalStrict(d, &global); err != nil {
				util.PrintErr(err.Error())
			}
		}
	}

	for _, c := range o.Configs {
		if err := filepath.Walk(c, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}

			if !util.HasExtension(path, yamlExt) || filepath.Base(path) == defaultsFilename {
				return nil
			}

			var local configuration.Configuration
			if d, err := ioutil.ReadFile(filepath.Join(filepath.Dir(path), defaultsFilename)); err == nil {
				if err := yaml.UnmarshalStrict(d, &local); err != nil {
					util.PrintErr(err.Error())
				}
			}

			f, err := ioutil.ReadFile(path)
			if err != nil {
				return nil
			}

			var c configuration.Configuration
			if err := yaml.UnmarshalStrict(f, &c); err != nil {
				util.PrintErr(err.Error())
				return nil
			}

			for _, t := range c.Transforms {
				if len(t.JobType) == 0 {
					t.JobType = defaultJobTypes
				}

				applyDefaultTransforms(&t, &c.Defaults, &local.Defaults, &global.Defaults)

				oc := options{
					EnvDenylistSet:    sets.NewString(t.EnvDenylist...),
					VolumeDenylistSet: sets.NewString(t.VolumeDenylist...),
					JobAllowlistSet:   sets.NewString(t.JobAllowlist...),
					JobDenylistSet:    sets.NewString(t.JobDenylist...),
					RepoAllowlistSet:  sets.NewString(t.RepoAllowlist...),
					RepoDenylistSet:   sets.NewString(t.RepoDenylist...),
					JobTypeSet:        sets.NewString(t.JobType...),
					Transform:         t,
				}

				if err := oc.validateOpts(); err != nil {
					util.PrintErrAndExit(err)
				}

				optsList = append(optsList, oc)
			}

			return nil
		}); err != nil {
			util.PrintErr(err.Error())
		}
	}

	return optsList
}

// validateOpts validates the command-line flags.
func (o *options) validateOpts() error {
	var err error

	for i, c := range o.Configs {
		if o.Configs[i], err = filepath.Abs(c); err != nil {
			return &util.ExitError{Message: fmt.Sprintf("--configs option invalid: %v.", o.Configs[i]), Code: 1}
		} else if !util.Exists(o.Configs[i]) {
			return &util.ExitError{Message: fmt.Sprintf("--configs option path does not exist: %v.", o.Configs[i]), Code: 1}
		} else if util.IsFile(o.Configs[i]) && !util.HasExtension(o.Configs[i], yamlExt) {
			return &util.ExitError{Message: fmt.Sprintf("--configs option path is not a yaml file: %v.", o.Configs[i]), Code: 1}
		}
	}

	if o.Global != "" {
		if o.Global, err = filepath.Abs(o.Global); err != nil {
			return &util.ExitError{Message: fmt.Sprintf("--global option invalid: %v.", o.Global), Code: 1}
		} else if !util.Exists(o.Global) {
			return &util.ExitError{Message: fmt.Sprintf("--global option path does not exist: %v.", o.Global), Code: 1}
		} else if util.IsFile(o.Global) && !util.HasExtension(o.Global, yamlExt) {
			return &util.ExitError{Message: fmt.Sprintf("--global option path is not a yaml file: %v.", o.Global), Code: 1}
		}
	}

	if len(o.Configs) == 0 {
		if len(o.OrgMap) == 0 {
			return &util.ExitError{Message: "-m, --mapping option is required.", Code: 1}
		}

		if o.Input, err = filepath.Abs(o.Input); err != nil {
			return &util.ExitError{Message: fmt.Sprintf("-i, --input option invalid: %v.", o.Input), Code: 1}
		}

		if o.Output, err = filepath.Abs(o.Output); err != nil {
			return &util.ExitError{Message: fmt.Sprintf("-o, --output option invalid: %v.", o.Output), Code: 1}
		}

		for i, c := range o.Presets {
			if o.Presets[i], err = filepath.Abs(c); err != nil {
				return &util.ExitError{Message: fmt.Sprintf("-p, --preset option invalid: %v.", o.Presets[i]), Code: 1}
			}
			if !util.Exists(o.Presets[i]) {
				return &util.ExitError{Message: fmt.Sprintf("-p, --preset option path does not exist: %v.", o.Presets[i]), Code: 1}
			}
			if util.IsFile(o.Presets[i]) && !util.HasExtension(o.Presets[i], yamlExt) {
				return &util.ExitError{Message: fmt.Sprintf("-p, --preset option path is not a yaml file: %v.", o.Presets[i]), Code: 1}
			}
		}
	}

	return nil
}

// applyDefaultTransforms defaults transform struct from left to right with decreasing precedence.
func applyDefaultTransforms(dst *configuration.Transform, srcs ...*configuration.Transform) {
	for _, src := range srcs {
		if dst.Annotations == nil {
			dst.Annotations = src.Annotations
		}
		if dst.Bucket == "" {
			dst.Bucket = src.Bucket
		}
		if dst.Cluster == "" {
			dst.Cluster = src.Cluster
		}
		if dst.Channel == "" {
			dst.Channel = src.Channel
		}
		if dst.SSHKeySecret == "" {
			dst.SSHKeySecret = src.SSHKeySecret
		}
		if dst.Modifier == "" {
			dst.Modifier = src.Modifier
		}
		if dst.ServiceAccount == "" {
			dst.ServiceAccount = src.ServiceAccount
		}
		if dst.Input == "" {
			dst.Input = src.Input
		}
		if dst.Output == "" {
			dst.Output = src.Output
		}
		if dst.Sort == "" {
			dst.Sort = src.Sort
		}
		if len(dst.ExtraRefs) == 0 {
			dst.ExtraRefs = src.ExtraRefs
		}
		if len(dst.Branches) == 0 {
			dst.Branches = src.Branches
		}
		if len(dst.BranchesOut) == 0 {
			dst.BranchesOut = src.BranchesOut
		}
		if len(dst.RefBranchOut) == 0 {
			dst.RefBranchOut = src.RefBranchOut
		}
		if len(dst.Presets) == 0 {
			dst.Presets = src.Presets
		}
		if len(dst.RerunOrgs) == 0 {
			dst.RerunOrgs = src.RerunOrgs
		}
		if len(dst.RerunUsers) == 0 {
			dst.RerunUsers = src.RerunUsers
		}
		if len(dst.EnvDenylist) == 0 {
			dst.EnvDenylist = src.EnvDenylist
		}
		if len(dst.VolumeDenylist) == 0 {
			dst.VolumeDenylist = src.VolumeDenylist
		}
		if len(dst.JobAllowlist) == 0 {
			dst.JobAllowlist = src.JobAllowlist
		}
		if len(dst.JobDenylist) == 0 {
			dst.JobDenylist = src.JobDenylist
		}
		if len(dst.RepoAllowlist) == 0 {
			dst.RepoAllowlist = src.RepoAllowlist
		}
		if len(dst.RepoDenylist) == 0 {
			dst.RepoDenylist = src.RepoDenylist
		}
		if len(dst.JobType) == 0 {
			dst.JobType = src.JobType
		}
		if len(dst.Selector) == 0 {
			dst.Selector = src.Selector
		}
		if len(dst.Labels) == 0 {
			dst.Labels = src.Labels
		}
		if len(dst.Env) == 0 {
			dst.Env = src.Env
		}
		if len(dst.OrgMap) == 0 {
			dst.OrgMap = src.OrgMap
		}
		if len(dst.RefOrgMap) == 0 {
			dst.RefOrgMap = src.RefOrgMap
		}
		if len(dst.HubMap) == 0 {
			dst.HubMap = src.HubMap
		}
		if dst.Tag == "" {
			dst.Tag = src.Tag
		}
		if !dst.DryRun {
			dst.DryRun = src.DryRun
		}
		if !dst.Refs {
			dst.Refs = src.Refs
		}
		if !dst.Resolve {
			dst.Resolve = src.Resolve
		}
		if !dst.SSHClone {
			dst.SSHClone = src.SSHClone
		}
		if !dst.OverrideSelector {
			dst.OverrideSelector = src.OverrideSelector
		}
		if !dst.AllowLongJobNames {
			dst.AllowLongJobNames = src.AllowLongJobNames
		}
		if !dst.Verbose {
			dst.Verbose = src.Verbose
		}
		if !dst.Clean {
			dst.Clean = src.Clean
		}
	}
}

// validateOrgRepo validates that the org and repo for a job pass validation and should be converted.
func validateOrgRepo(o options, org string, repo string) bool {
	_, hasOrg := o.OrgMap[org]

	if !hasOrg || o.RepoDenylistSet.Has(repo) || (len(o.RepoAllowlistSet) > 0 && !o.RepoAllowlistSet.Has(repo)) {
		return false
	}

	return true
}

// validateJob validates that the job passes validation and should be converted.
func validateJob(o options, name string, patterns []string, jType string) bool {
	if hasMatch(name, o.JobDenylistSet.List()) || (len(o.JobAllowlistSet) > 0 && !hasMatch(name, o.JobAllowlistSet.List())) ||
		!isMatchBranch(o, patterns) || !o.JobTypeSet.Has(jType) {
		return false
	}

	return true
}

// isMatchBranch validates that the branch for a job passes validation and should be converted.
func isMatchBranch(o options, patterns []string) bool {
	if len(o.Branches) == 0 {
		return true
	}

	for _, branch := range o.Branches {
		if hasMatch(branch, patterns) {
			return true
		}
	}

	return false
}

// hasMatch checks if there is any match in patterns for the given name.
func hasMatch(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if regexp.MustCompile(pattern).MatchString(name) {
			return true
		}
	}
	return false
}

// allRefs returns true if all predicate function returns true for the array of ref.
func allRefs(array []prowjob.Refs, predicate func(val prowjob.Refs, idx int) bool) bool {
	for idx, item := range array {
		if !predicate(item, idx) {
			return false
		}
	}
	return true
}

// convertOrgRepoStr translates the provided job org and repo based on the specified org mapping.
func convertOrgRepoStr(o options, s string) string {
	org, repo := util.SplitOrgRepo(s)

	valid := validateOrgRepo(o, org, repo)

	if !valid {
		return ""
	}

	return strings.Join([]string{o.OrgMap[org], repo}, "/")
}

// combinePresets reads a list of paths and aggregates the presets.
func combinePresets(paths []string) []config.Preset {
	presets := []config.Preset{}

	if len(paths) == 0 {
		return presets
	}

	for _, p := range paths {
		c, err := config.ReadJobConfig(p)
		if err != nil {
			util.PrintErr(err.Error())
			continue
		}
		presets = append(presets, c.Presets...)
	}

	return presets
}

// mergePreset merges a preset into a job Spec based on defined labels.
func mergePreset(labels map[string]string, job *config.JobBase, preset config.Preset) {
	for l, v := range preset.Labels {
		if v2, exists := labels[l]; !exists || v != v2 {
			return
		}
	}

	for _, env := range preset.Env {
	econtainer:
		for i := range job.Spec.Containers {
			for j := range job.Spec.Containers[i].Env {
				if job.Spec.Containers[i].Env[j].Name == env.Name {
					job.Spec.Containers[i].Env[j].Value = env.Value
					continue econtainer
				}
			}

			job.Spec.Containers[i].Env = append(job.Spec.Containers[i].Env, env)
		}
	}

volume:
	for _, vol := range preset.Volumes {

		for i := range job.Spec.Volumes {
			if job.Spec.Volumes[i].Name == vol.Name {
				job.Spec.Volumes[i] = vol
				continue volume
			}
		}

		job.Spec.Volumes = append(job.Spec.Volumes, vol)
	}

	for _, volm := range preset.VolumeMounts {
	vcontainer:
		for i := range job.Spec.Containers {
			for j := range job.Spec.Containers[i].VolumeMounts {
				if job.Spec.Containers[i].VolumeMounts[j].Name == volm.Name {
					job.Spec.Containers[i].VolumeMounts[j] = volm
					continue vcontainer
				}
			}

			job.Spec.Containers[i].VolumeMounts = append(job.Spec.Containers[i].VolumeMounts, volm)
		}
	}
}

// resolvePresets resolves all preset for a particular job Spec based on defined labels.
func resolvePresets(o options, labels map[string]string, job *config.JobBase, presets []config.Preset) {
	if !o.Resolve {
		return
	}

	if job.Spec != nil {
		for _, preset := range presets {
			mergePreset(labels, job, preset)
		}
	}
}

// pruneJobBase prunes denylisted fields from the job Spec.
func pruneJobBase(o options, job *config.JobBase) {
	if job.Spec != nil {
		if len(o.VolumeDenylistSet) > 0 {
			pruneVolumes(o.VolumeDenylistSet, job)
		}
		if len(o.EnvDenylistSet) > 0 {
			pruneEnvs(o.EnvDenylistSet, job)
		}
	}
}

// pruneEnvs prunes denylisted Env fields.
func pruneEnvs(denylist sets.String, job *config.JobBase) {
	for i := range job.Spec.Containers {
		var envs []v1.EnvVar

		for _, env := range job.Spec.Containers[i].Env {
			if denylist.Has(env.Name) {
				continue
			}
			envs = append(envs, env)
		}
		job.Spec.Containers[i].Env = envs
	}
}

// pruneVolumes prunes denylisted Volume and VolueMount fields.
func pruneVolumes(denylist sets.String, job *config.JobBase) {
	var volumes []v1.Volume

	for _, vol := range job.Spec.Volumes {
		if denylist.Has(vol.Name) {
			continue
		}
		volumes = append(volumes, vol)
	}
	job.Spec.Volumes = volumes

	for i := range job.Spec.Containers {
		var volumeMounts []v1.VolumeMount

		for _, volm := range job.Spec.Containers[i].VolumeMounts {
			if denylist.Has(volm.Name) {
				continue
			}
			volumeMounts = append(volumeMounts, volm)
		}
		job.Spec.Containers[i].VolumeMounts = volumeMounts
	}
}

// updateJobName updates the jobs Name fields based on provided inputs.
func updateJobName(o options, job *config.JobBase) {
	suffix := ""

	if o.Modifier != "" {
		suffix = jobnameSeparator + o.Modifier
	}

	if !o.AllowLongJobNames {
		maxNameLen := maxLabelLen - len(suffix)

		if len(job.Name) > maxNameLen {
			job.Name = job.Name[:maxNameLen]
		}
	}

	job.Name += suffix
}

// updateBrancher updates the jobs Brancher fields based on provided inputs.
func updateBrancher(o options, job *config.Brancher) {
	if len(o.BranchesOut) == 0 {
		return
	}

	job.Branches = o.BranchesOut
}

// updateUtilityConfig updates the jobs UtilityConfig fields based on provided inputs.
func updateUtilityConfig(o options, job *config.UtilityConfig) {
	if o.Bucket == "" && o.SSHKeySecret == "" {
		return
	}

	if job.DecorationConfig == nil {
		job.DecorationConfig = &prowjob.DecorationConfig{}
	}

	updateGCSConfiguration(o, job.DecorationConfig)
	updateSSHKeySecrets(o, job.DecorationConfig)
}

// updateGCSConfiguration updates the jobs GCSConfiguration fields based on provided inputs.
func updateGCSConfiguration(o options, job *prowjob.DecorationConfig) {
	if o.Bucket == "" {
		return
	}

	if job.GCSConfiguration == nil {
		job.GCSConfiguration = &prowjob.GCSConfiguration{
			Bucket: o.Bucket,
		}
	} else {
		job.GCSConfiguration.Bucket = o.Bucket
	}
}

// updateSSHKeySecrets updates the jobs SSHKeySecrets fields based on provided inputs.
func updateSSHKeySecrets(o options, job *prowjob.DecorationConfig) {
	if o.SSHKeySecret == "" {
		return
	}

	if job.SSHKeySecrets == nil {
		job.SSHKeySecrets = []string{o.SSHKeySecret}
	} else {
		job.SSHKeySecrets = append(job.SSHKeySecrets, o.SSHKeySecret)
	}
}

// updateGerritReportingLabels updates the gerrit reporting labels based on provided inputs.
func updateGerritReportingLabels(o options, skipReport, optional bool, labels map[string]string) {
	if o.SupportGerritReporting && !skipReport {
		if !optional {
			// For non-optional jobs, only add the label if it's not configured,
			// this allows us defining internal jobs that report to a different label.
			if _, ok := labels[gerritReportLabel]; !ok {
				labels[gerritReportLabel] = "Verified"
			}
		} else {
			labels[gerritReportLabel] = "Advisory"
		}
	} else {
		delete(labels, gerritReportLabel)
	}
}

// updateReporterConfig updates the jobs ReporterConfig fields based on provided inputs.
func updateReporterConfig(o options, job *config.JobBase) {
	if o.Channel == "" {
		return
	}

	if job.ReporterConfig == nil {
		job.ReporterConfig = &prowjob.ReporterConfig{}
	}

	job.ReporterConfig.Slack = &prowjob.SlackReporterConfig{Channel: o.Channel}
}

// updateRerunAuthConfig updates the jobs RerunAuthConfig fields based on provided inputs.
func updateRerunAuthConfig(o options, job *config.JobBase) {
	if len(o.RerunOrgs) == 0 && len(o.RerunUsers) == 0 {
		return
	}

	// The original job `RerunAuthConfig` is overwritten with the user-defined values.
	job.RerunAuthConfig = &prowjob.RerunAuthConfig{
		GitHubOrgs:  o.RerunOrgs,
		GitHubUsers: o.RerunUsers,
	}
}

// updateLabels updates the jobs Labels fields based on provided inputs.
func updateLabels(o options, job *config.JobBase) {
	if len(o.Labels) == 0 {
		return
	}

	if job.Labels == nil {
		job.Labels = make(map[string]string)
	}

	for labelK, labelV := range o.Labels {
		job.Labels[labelK] = labelV
	}
}

// updateNodeSelector updates the jobs NodeSelector fields based on provided inputs.
func updateNodeSelector(o options, job *config.JobBase) {
	if o.OverrideSelector {
		job.Spec.NodeSelector = make(map[string]string)
	}

	if len(o.Selector) == 0 {
		return
	}

	if job.Spec.NodeSelector == nil {
		job.Spec.NodeSelector = make(map[string]string)
	}

	for selK, selV := range o.Selector {
		job.Spec.NodeSelector[selK] = selV
	}
}

// updateEnvs updates the jobs Env fields based on provided inputs.
func updateEnvs(o options, job *config.JobBase) {
	if len(o.Env) == 0 {
		return
	}

	envKs := util.SortedKeys(o.Env)

	for _, envK := range envKs {
	container:
		for i := range job.Spec.Containers {

			for j := range job.Spec.Containers[i].Env {
				if job.Spec.Containers[i].Env[j].Name == envK {
					job.Spec.Containers[i].Env[j].Value = o.Env[envK]
					continue container
				}
			}

			job.Spec.Containers[i].Env = append(job.Spec.Containers[i].Env, v1.EnvVar{Name: envK, Value: o.Env[envK]})
		}
	}
}

// updateJobBase updates the jobs JobBase fields based on provided inputs to work with private repositories.
func updateJobBase(o options, job *config.JobBase, orgrepo string) {
	if len(o.Annotations) != 0 {
		job.Annotations = o.Annotations
	}

	if o.SSHClone && orgrepo != "" {
		job.CloneURI = fmt.Sprintf("git@%s:%s.git", gitHost, orgrepo)
	}

	if o.Cluster != "" && o.Cluster != defaultCluster {
		job.Cluster = o.Cluster
	}

	updateJobName(o, job)
	updateReporterConfig(o, job)
	updateRerunAuthConfig(o, job)
	updateLabels(o, job)
	updateNodeSelector(o, job)
	updateEnvs(o, job)
	updateServiceAccount(o, job)
}

// updateServiceAccount updates the jobs ServiceAccountName fields based on provided inputs.
func updateServiceAccount(o options, job *config.JobBase) {
	if o.ServiceAccount == "" || job.Spec.ServiceAccountName == "" {
		return
	}

	job.Spec.ServiceAccountName = o.ServiceAccount
}

// updateExtraRefs updates the jobs ExtraRefs fields based on provided inputs to work with private repositories.
func updateExtraRefs(o options, job *config.UtilityConfig) {
	for i, ref := range job.ExtraRefs {
		org, repo := ref.Org, ref.Repo

		if o.Refs || validateOrgRepo(o, org, repo) {
			// Try to transform known ref org mappings first.
			if newOrg, ok := o.RefOrgMap[org]; ok {
				org = newOrg
				job.ExtraRefs[i].CloneURI = fmt.Sprintf("https://%s/%s", org, repo)
				// Then try to transform general org mappings.
			} else if newOrg, ok := o.OrgMap[org]; ok {
				org = newOrg
			}
			job.ExtraRefs[i].Org = org
			if o.SSHClone {
				job.ExtraRefs[i].CloneURI = fmt.Sprintf("git@%s:%s/%s.git", gitHost, org, repo)
			}
			if o.RefBranchOut != "" {
				job.ExtraRefs[i].BaseRef = o.RefBranchOut
			}
		}
	}
	if len(o.ExtraRefs) > 0 {
		job.ExtraRefs = append(job.ExtraRefs, o.ExtraRefs...)
	}
}

// updateHubs updates the docker hubs for container images
func updateHubs(o options, job *config.JobBase) {
	for i := range job.Spec.Containers {
		tag, _ := dockername.NewTag(job.Spec.Containers[i].Image)
		baseref := tag.Context().Name()
		for in, out := range o.HubMap {
			baseref = strings.ReplaceAll(
				baseref,
				in,
				out,
			)
		}
		newTag, _ := dockername.NewTag(fmt.Sprintf("%s:%s", baseref, tag.TagStr()))
		job.Spec.Containers[i].Image = newTag.Name()
	}
}

// updateTags forces an override of the docker tags for container images
func updateTags(o options, job *config.JobBase) {
	if o.Tag == "" {
		return
	}
	for i := range job.Spec.Containers {
		tag, _ := dockername.NewTag(job.Spec.Containers[i].Image)
		baseref := tag.Context().Name()
		job.Spec.Containers[i].Image = baseref + ":" + o.Tag
	}
}

// sortJobs sorts jobs based on a provided sort order.
func sortJobs(o options, pre map[string][]config.Presubmit, post map[string][]config.Postsubmit, per []config.Periodic) {
	if o.Sort == "" {
		return
	}

	choices := strings.Join([]string{string(ascending), string(descending)}, "|")
	matches := regexp.MustCompile(`^(` + choices + `)(?:ending)?$`).FindStringSubmatch(o.Sort)
	if len(matches) < 2 {
		return
	}

	var comparator func(a, b string) bool

	switch sortOrder(matches[1]) {
	case ascending:
		comparator = func(a, b string) bool {
			return a < b
		}
	case descending:
		comparator = func(a, b string) bool {
			return a > b
		}
	}

	for _, c := range pre {
		sort.Slice(c, func(a, b int) bool {
			return comparator(c[a].Name, c[b].Name)
		})
	}

	for _, c := range post {
		sort.Slice(c, func(a, b int) bool {
			return comparator(c[a].Name, c[b].Name)
		})
	}

	sort.Slice(per, func(a, b int) bool {
		return comparator(per[a].Name, per[b].Name)
	})
}

// getOutPath derives the output path from the specified input directory and current path.
func getOutPath(o options, p string, in string, branches, branchesOut []string) string {
	segments := strings.FieldsFunc(strings.TrimPrefix(p, in), func(c rune) bool { return c == '/' })

	var (
		org  string
		repo string
		file string
	)

	switch {
	case util.HasExtension(o.Output, yamlExt):
		return o.Output
	case len(segments) >= 3:
		org = segments[len(segments)-3]
		repo = segments[len(segments)-2]
		file = segments[len(segments)-1]
		if newOrg, ok := o.OrgMap[org]; ok {
			filename := util.RenameFile(`^`+util.NormalizeOrg(org, filenameSeparator)+`\b`, file, util.NormalizeOrg(newOrg, filenameSeparator))
			if len(branchesOut) > 0 {
				filename = util.NormalizeConfigName(strings.ReplaceAll(filename, branches[0], branchesOut[0]))
			}
			return filepath.Join(o.Output, util.GetTopLevelOrg(newOrg), repo, filename)
		}
	case len(segments) == 2:
		org = segments[len(segments)-2]
		file = segments[len(segments)-1]
		if newOrg, ok := o.OrgMap[org]; ok {
			filename := util.RenameFile(`^`+util.NormalizeOrg(org, filenameSeparator)+`\b`, file, util.NormalizeOrg(newOrg, filenameSeparator))
			return filepath.Join(o.Output, util.GetTopLevelOrg(newOrg), filename)
		}
	case len(segments) == 1:
		file = segments[len(segments)-1]
		if !strings.HasPrefix(file, o.Modifier) {
			return filepath.Join(o.Output, o.Modifier+filenameSeparator+file)
		}
	case len(segments) == 0:
		file = filepath.Base(in)
		if !strings.HasPrefix(file, o.Modifier) {
			return filepath.Join(o.Output, o.Modifier+filenameSeparator+file)
		}
	}

	return ""
}

// cleanOutFile deletes a path and any children.
func cleanOutFile(p string) {
	if err := os.RemoveAll(p); err != nil {
		util.PrintErr(fmt.Sprintf("unable to clean file %v: %v.", p, err))
	}
}

func handleRecover() {
	if r := recover(); r != nil {
		switch t := r.(type) {
		case string:
			util.PrintErrAndExit(errors.New(t))
		case error:
			util.PrintErrAndExit(t)
		default:
			util.PrintErrAndExit(errors.New("unknown panic"))
		}
	}
}

// writeOutFile writes all jobs definitions to the designated output path.
func writeOutFile(o options, p string, pre map[string][]config.Presubmit, post map[string][]config.Postsubmit, per []config.Periodic) {
	if len(pre) == 0 && len(post) == 0 && len(per) == 0 {
		return
	}

	combinedPre := map[string][]config.Presubmit{}
	combinedPost := map[string][]config.Postsubmit{}
	combinedPer := []config.Periodic{}

	existingJobs, err := config.ReadJobConfig(p)
	if err == nil {
		if existingJobs.PresubmitsStatic != nil {
			combinedPre = existingJobs.PresubmitsStatic
		}
		if existingJobs.PostsubmitsStatic != nil {
			combinedPost = existingJobs.PostsubmitsStatic
		}
		if existingJobs.Periodics != nil {
			combinedPer = existingJobs.Periodics
		}
	}

	// Combine presubmits
	for orgrepo, newPre := range pre {
		if oldPre, exists := combinedPre[orgrepo]; exists {
			combinedPre[orgrepo] = append(oldPre, newPre...)
		} else {
			combinedPre[orgrepo] = newPre
		}
	}

	// Combine postsubmits
	for orgrepo, newPost := range post {
		if oldPost, exists := combinedPost[orgrepo]; exists {
			combinedPost[orgrepo] = append(oldPost, newPost...)
		} else {
			combinedPost[orgrepo] = newPost
		}
	}

	// Combine periodics
	combinedPer = append(combinedPer, per...)

	// Sort presubmits, postsubmits, and periodics
	sortJobs(o, combinedPre, combinedPost, combinedPer)

	jobConfig := config.JobConfig{}

	err = jobConfig.SetPresubmits(combinedPre)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to set presubmits for path %v: %v.", p, err))
	}

	err = jobConfig.SetPostsubmits(combinedPost)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to set postsubmits for path %v: %v.", p, err))
	}

	jobConfig.Periodics = combinedPer

	jobConfigYaml, err := yaml.Marshal(jobConfig)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to marshal job config output directory: %v.", err))
		return
	}

	outBytes := []byte(autogenHeader)
	outBytes = append(outBytes, jobConfigYaml...)

	dir := filepath.Dir(p)

	err = os.MkdirAll(dir, os.ModePerm)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to create output directory %v: %v.", dir, err))
	}

	err = ioutil.WriteFile(p, outBytes, 0o644)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to write jobs to path %v: %v.", p, err))
	}
}

// generateJobs generates jobs based on the specified options.
func generateJobs(o options) {
	presets := combinePresets(o.Presets)

	if err := filepath.Walk(o.Input, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		absPath, _ := filepath.Abs(p)

		if !util.HasExtension(absPath, yamlExt) {
			return nil
		}

		outPath := getOutPath(o, absPath, o.Input, o.Branches, o.BranchesOut)
		if outPath == "" {
			return nil
		}
		if o.Clean {
			cleanOutFile(outPath)
		}

		jobs, err := config.ReadJobConfig(absPath)
		if err != nil {
			return nil
		}

		presubmit := map[string][]config.Presubmit{}
		postsubmit := map[string][]config.Postsubmit{}
		periodic := []config.Periodic{}

		// Presubmits
		for orgrepo, pre := range jobs.PresubmitsStatic {
			orgrepo = convertOrgRepoStr(o, orgrepo)
			if orgrepo == "" {
				continue
			}

			for _, job := range pre {
				valid := validateJob(o, job.Name, job.Branches, "presubmit")
				if !valid {
					continue
				}

				updateExtraRefs(o, &job.UtilityConfig)
				updateJobBase(o, &job.JobBase, orgrepo)
				updateBrancher(o, &job.Brancher)
				updateUtilityConfig(o, &job.UtilityConfig)
				updateGerritReportingLabels(o, job.SkipReport, job.Optional, job.Labels)
				resolvePresets(o, job.Labels, &job.JobBase, append(presets, jobs.Presets...))
				pruneJobBase(o, &job.JobBase)
				updateHubs(o, &job.JobBase)
				updateTags(o, &job.JobBase)

				presubmit[orgrepo] = append(presubmit[orgrepo], job)
			}
		}

		// Postsubmits
		for orgrepo, post := range jobs.PostsubmitsStatic {
			orgrepo = convertOrgRepoStr(o, orgrepo)
			if orgrepo == "" {
				continue
			}

			for _, job := range post {
				valid := validateJob(o, job.Name, job.Branches, "postsubmit")
				if !valid {
					continue
				}

				updateExtraRefs(o, &job.UtilityConfig)
				updateJobBase(o, &job.JobBase, orgrepo)
				updateBrancher(o, &job.Brancher)
				updateUtilityConfig(o, &job.UtilityConfig)
				resolvePresets(o, job.Labels, &job.JobBase, append(presets, jobs.Presets...))
				pruneJobBase(o, &job.JobBase)
				updateHubs(o, &job.JobBase)
				updateTags(o, &job.JobBase)

				postsubmit[orgrepo] = append(postsubmit[orgrepo], job)
			}
		}

		// Periodic
		for _, job := range jobs.Periodics {
			if len(job.ExtraRefs) == 0 {
				continue
			}

			if allRefs(job.ExtraRefs, func(val prowjob.Refs, idx int) bool {
				return !validateOrgRepo(o, val.Org, val.Repo)
			}) {
				continue
			}

			branches := make([]string, 0)
			for _, ref := range job.ExtraRefs {
				if validateOrgRepo(o, ref.Org, ref.Repo) {
					branches = append(branches, ref.BaseRef)
				}
			}
			if !validateJob(o, job.Name, branches, "periodic") {
				continue
			}

			updateExtraRefs(o, &job.UtilityConfig)
			updateJobBase(o, &job.JobBase, "")
			updateUtilityConfig(o, &job.UtilityConfig)
			resolvePresets(o, job.Labels, &job.JobBase, append(presets, jobs.Presets...))
			pruneJobBase(o, &job.JobBase)
			updateHubs(o, &job.JobBase)
			updateTags(o, &job.JobBase)

			periodic = append(periodic, job)
		}

		if o.Verbose {
			fmt.Printf("write %d presubmits, %d postsubmits, and %d periodics to path %v\n", len(presubmit), len(postsubmit), len(periodic), outPath)
		}

		if !o.DryRun {
			writeOutFile(o, outPath, presubmit, postsubmit, periodic)
		}

		return nil
	}); err != nil {
		util.PrintErr(err.Error())
	}
}

// main entry point.
func Main() {
	defer handleRecover()

	var o options

	o.parseOpts()

	if err := o.validateOpts(); err != nil {
		util.PrintErrAndExit(err)
	}

	optsList := []options{o}
	optsList = append(optsList, o.parseConfiguration()...)

	for _, o := range optsList {
		generateJobs(o)
	}
}

func main() {
	Main()
}
