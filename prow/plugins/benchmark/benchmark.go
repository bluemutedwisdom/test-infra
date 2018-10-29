/*
Copyright 2016 The Kubernetes Authors.

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

package benchmark

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
	"k8s.io/test-infra/prow/repoowners"
)

const pluginName = "benchmark"
const startBenchmarkJobName = "start-benchmark"
const cancelBenchmarkJobName = "cancel-benchmark"
const prowJobPRNumber = "PR_NUMBER"
const prowJobRelease = "RELEASE"
const maxTries = 50

var (
	benchmarkLabel        = "benchmark"
	benchmarkPendingLabel = "pending-benchmark-job"
	benchmarkRe           = regexp.MustCompile(`(?mi)^/benchmark\s*(master|[0-9]+\.[0-9]+\.[0-9]+\S*)?\s*$`)
	benchmarkCancelRe     = regexp.MustCompile(`(?mi)^/benchmark\s+cancel\s*$`)
	prombenchURL          = "http://prombench.prometheus.io"
)

func init() {
	plugins.RegisterIssueCommentHandler(pluginName, handleIssueComment, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	// The Config field is omitted because this plugin is not configurable.
	pluginHelp := &pluginhelp.PluginHelp{
		Description: "The benchmark plugin starts prometheus benchmarking tool(prombench).",
	}
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/benchmark master or /benchmark <RELEASE_NUMBER>(ex:2.3.0-rc.1 | Default: master)",
		Description: "Starts prometheus benchmarking tool. With `release` current master will be compared with previous release. With `pr`, PR will be compared with current master.",
		Featured:    true,
		WhoCanUse:   "Members of the same github org.",
		Examples:    []string{"/benchmark", "/benchmark master", "/benchmark 2.3.0-rc.1", "/benchmark cancel"},
	})
	return pluginHelp, nil
}

type githubClient interface {
	IsMember(owner, login string) (bool, error)
	AddLabel(owner, repo string, number int, label string) error
	AssignIssue(owner, repo string, number int, assignees []string) error
	CreateComment(owner, repo string, number int, comment string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	GetRef(org, repo, ref string) (string, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
	DeleteComment(org, repo string, ID int) error
	BotName() (string, error)
}

type kubeClient interface {
	CreateProwJob(kube.ProwJob) (kube.ProwJob, error)
	ListProwJobs(string) ([]kube.ProwJob, error)
	GetProwJob(name string) (kube.ProwJob, error)
}

type client struct {
	GitHubClient githubClient
	KubeClient   kubeClient
	Config       *config.Config
	Logger       *logrus.Entry
}

func getClient(pc plugins.PluginClient) client {
	return client{
		GitHubClient: pc.GitHubClient,
		Config:       pc.Config,
		KubeClient:   pc.KubeClient,
		Logger:       pc.Logger,
	}
}

//function to handle GitHub commentEvent
func handleIssueComment(pc plugins.PluginClient, ic github.IssueCommentEvent) error {
	return handle(getClient(pc), pc.OwnersClient, ic)
}

func handle(c client, ownersClient repoowners.Interface, ic github.IssueCommentEvent) error {
	// Only consider PRs and new comments.
	if ic.Issue.PullRequest == nil || ic.Action != github.IssueCommentActionCreated {
		return nil
	}

	org := ic.Repo.Owner.Login
	repo := ic.Repo.Name
	number := ic.Issue.Number

	c.Logger.Debugf("Checking benchmark trigger keywords to decide deploy or clean")
	wantBenchmark := false
	if benchmarkRe.MatchString(ic.Comment.Body) {
		wantBenchmark = true
	} else if benchmarkCancelRe.MatchString(ic.Comment.Body) {
		wantBenchmark = false
	} else {
		return nil
	}

	c.Logger.Debugf("Checking if author is member to the organisation.")
	if strings.ToLower(ic.Comment.AuthorAssociation) != github.RoleMember {
		resp := "Benchmarking is restricted to org members."
		c.Logger.Infof("Reply to /benchmark request with comment: \"%s\".", resp)
		return c.GitHubClient.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, resp))
	}

	c.Logger.Debugf("Checking which version of Prometheus to benchmark PR with.")
	releaseVersion := ""
	if wantBenchmark {
		group := benchmarkRe.FindStringSubmatch(ic.Comment.Body)
		version := strings.TrimSpace(group[1])

		if version == "" || version == "master" {
			releaseVersion = "master"
		} else {
			releaseVersion = "v" + version
		}
	}

	c.Logger.Debugf("Checking any pending or reduntant jobs using PR labels")
	hasBenchmarkLabel := false
	hasBenchmarkPendingLabel := false
	labels, err := c.GitHubClient.GetIssueLabels(org, repo, number)
	if err != nil {
		return fmt.Errorf("Failed to get the labels on %s/%s#%d %v.", org, repo, number, err)
	}
	for _, candidate := range labels {
		if candidate.Name == benchmarkLabel {
			hasBenchmarkLabel = true
		}
		if candidate.Name == benchmarkPendingLabel {
			hasBenchmarkPendingLabel = true
		}
	}

	if hasBenchmarkPendingLabel {
		return c.GitHubClient.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, "Looks like a job is already lined up for this PR.<br/> Please try again once all pending jobs have finished :smiley:"))
	}

	if wantBenchmark {
		if !hasBenchmarkLabel {
			c.Logger.Infof("Adding Benchmark label.")
			if err := c.GitHubClient.AddLabel(org, repo, number, benchmarkLabel); err != nil {
				return err
			}
		} else {
			return c.GitHubClient.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, "Looks like benchmarking is already running for this PR.<br/> You can cancel benchmarking by commenting `/benchmark cancel`. :smiley:"))
		}

		comment := fmt.Sprintf(`Welcome to Prometheus Benchmarking Tool.

The two prometheus versions that will be compared are _**pr-%d**_ and _**%s**_

The logs can be viewed at the links provided in the GitHub check blocks at the end of this conversation

After successfull deployment, the benchmarking metrics can be viewed at :
- [prometheus-meta](%s/prometheus-meta) - label **{namespace="prombench-%d"}**
- [grafana](%s/grafana) - template-variable **"pr-number" : %d**

The Prometheus servers being benchmarked can be viewed at :
- PR - [prombench.prometheus.io/%d/prometheus-pr](%s/%d/prometheus-pr)
- %s - [prombench.prometheus.io/%d/prometheus-release](%s/%d/prometheus-release)

To stop the benchmark process comment **/benchmark cancel** .`, number, releaseVersion, prombenchURL, number, prombenchURL, number,
			number, prombenchURL, number, releaseVersion, number, prombenchURL, number)

		c.GitHubClient.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, comment))
		err := triggerBenchmarkJob(c, ic, startBenchmarkJobName, cancelBenchmarkJobName, releaseVersion, fmt.Sprintf("pr-%d", number))
		if err != nil {
			c.GitHubClient.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, fmt.Sprintf("Creation of prombench prowjob failed: %v", err)))
			c.GitHubClient.RemoveLabel(org, repo, number, benchmarkLabel)
			return fmt.Errorf("Failed to create prowjob to start-benchmark for release %v.", err)
		}
	} else {
		if !hasBenchmarkLabel {
			return c.GitHubClient.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, "Looks like benchmarking is not going on for this PR.<br/> You can start benchmarking by commenting `/benchmark master or /benchmark <RELEASE_NUMBER>(ex:2.3.0-rc.1 | Default: master)` :smiley:"))
		}
		///benchmark cancel does not require {{ .RELEASE }} template because just prombench namespace & cluster-role-binding need to be deleted to clean deployment
		//That's why "temp-release" is given as releaseVersion
		err := triggerBenchmarkJob(c, ic, cancelBenchmarkJobName, startBenchmarkJobName, "temp-release", fmt.Sprintf("pr-%d", number))
		if err != nil {
			c.GitHubClient.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, fmt.Sprintf("Deletion of prombench failed: %v", err)))
			return fmt.Errorf("Failed to create prowjob to cancel-benchmark %v.", err)
		}
		return c.GitHubClient.RemoveLabel(org, repo, number, benchmarkLabel)
	}
	return nil
}

//Function to start start-benchmark | cancel-benchmark
//This adds appropriate args in the pod spec
func triggerBenchmarkJob(c client, ic github.IssueCommentEvent, jobName string, otherJobName string, releaseVersion string, prVersion string) error {

	//otherJobName is the job for which we need to wait before starting prowjob for jobname
	err := waitForOtherBenchmarkJobToEnd(c, ic, otherJobName, jobName)
	if err != nil {
		return err
	}
	c.Logger.Infof("All pending %s jobs have finished", jobName)

	org := ic.Repo.Owner.Login
	repo := ic.Repo.Name
	number := ic.Issue.Number

	pr, err := c.GitHubClient.GetPullRequest(org, repo, number)
	if err != nil {
		return err
	}

	baseSHA, err := c.GitHubClient.GetRef(org, repo, "heads/"+pr.Base.Ref)
	if err != nil {
		return err
	}

	kr := kube.Refs{
		Org:     org,
		Repo:    repo,
		BaseRef: pr.Base.Ref,
		BaseSHA: baseSHA,
		Pulls: []kube.Pull{
			{
				Number: number,
				Author: pr.User.Login,
				SHA:    pr.Head.SHA,
			},
		},
	}

	var benchmark config.Presubmit
	for _, job := range c.Config.Presubmits[pr.Base.Repo.FullName] {
		if job.Name == jobName {
			c.Logger.Debugf("Adding args %s , %s to %s prowjob", releaseVersion, jobName, strconv.Itoa(number))

			job.Spec.Containers[0].Args = append(job.Spec.Containers[0].Args, fmt.Sprintf("%s=%s", prowJobRelease, releaseVersion))
			job.Spec.Containers[0].Args = append(job.Spec.Containers[0].Args, fmt.Sprintf("%s=%s", prowJobPRNumber, strconv.Itoa(number)))
			benchmark = job
			break
		}
	}

	labels := make(map[string]string)
	for k, v := range benchmark.Labels {
		labels[k] = v
	}

	labels[github.EventGUID] = ic.GUID

	pj := pjutil.NewProwJob(pjutil.PresubmitSpec(benchmark, kr), labels)
	c.Logger.WithFields(pjutil.ProwJobFields(&pj)).Info("Creating a new prowjob to ", jobName)
	if _, err := c.KubeClient.CreateProwJob(pj); err != nil {
		c.Logger.Infof("Failed to Create %s ProwJob %v.", jobName, err)
		return err
	}
	return nil
}

func waitForOtherBenchmarkJobToEnd(c client, ic github.IssueCommentEvent, waitForJob string, newJob string) error {
	org := ic.Repo.Owner.Login
	repo := ic.Repo.Name
	number := ic.Issue.Number

	defer c.GitHubClient.RemoveLabel(org, repo, number, benchmarkPendingLabel) //remove label irrespective of function status to not block future jobs

	pjs, err := c.KubeClient.ListProwJobs("")
	if err != nil {
		return err
	}

	pendingJobName := ""
	prNumberArg := fmt.Sprintf("%s=%s", prowJobPRNumber, strconv.Itoa(number))
ProwJobLoop:
	// check if other pending/triggered job is already running on this PR by comparing arg PR_NUMBER
	for _, pj := range pjs {
		if pj.Status.State == kube.TriggeredState || pj.Status.State == kube.PendingState {
			if pj.Spec.Job == waitForJob {
				for _, e := range pj.Spec.PodSpec.Containers[0].Args {
					if e == prNumberArg {
						c.Logger.Infof("Before starting %s, need to wait for %s Job.", newJob, waitForJob)
						pendingJobName = pj.Name
						break ProwJobLoop
					}
				}
			}
		}
	}

	if pendingJobName != "" {

		c.GitHubClient.AddLabel(org, repo, number, benchmarkPendingLabel)
		c.GitHubClient.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, fmt.Sprintf("Looks like %s job is already running on this PR. Will start %s job once ongoing job is completed", waitForJob, newJob)))

		c.Logger.Infof("Starting to wait for job %s:%s", waitForJob, pendingJobName)
		for i := 0; i < maxTries; i++ {
			pj, err := c.KubeClient.GetProwJob(pendingJobName)

			if err != nil {
				return fmt.Errorf("Failed to get ProwJob %s to end %s: %v", pendingJobName, waitForJob, err)
			}

			if pj.Status.State == kube.TriggeredState || pj.Status.State == kube.PendingState {
				c.Logger.Debugf("%d: %s is ongoing. Retrying after 30 seconds.", i, waitForJob)
				retry := time.Second * 30
				time.Sleep(retry)
			} else {
				return nil
			}
		}
		return fmt.Errorf("Ongoing %s job was not finished after trying for %d times.", waitForJob, maxTries)
	}
	return nil
}
