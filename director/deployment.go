package director

import (
	"encoding/json"
	"fmt"
	"net/http"
	gourl "net/url"
	"strings"

	bosherr "github.com/cloudfoundry/bosh-utils/errors"
)

type DeploymentImpl struct {
	client Client

	name        string
	cloudConfig string

	manifest string

	releases  []Release
	stemcells []Stemcell

	fetched  bool
	fetchErr error
}

type ExportReleaseResult struct {
	BlobstoreID string
	SHA1        string
}

type ExportReleaseResp struct {
	BlobstoreID string `json:"blobstore_id"`
	SHA1        string `json:"sha1"`
}

type LogsResult struct {
	BlobstoreID string
	SHA1        string
}

func (d DeploymentImpl) Name() string { return d.name }

func (d *DeploymentImpl) CloudConfig() (string, error) {
	d.fetch()
	return d.cloudConfig, d.fetchErr
}

func (d *DeploymentImpl) Releases() ([]Release, error) {
	d.fetch()
	return d.releases, d.fetchErr
}

func (d *DeploymentImpl) Stemcells() ([]Stemcell, error) {
	d.fetch()
	return d.stemcells, d.fetchErr
}

func (d *DeploymentImpl) fetch() {
	if d.fetched {
		return
	}

	resps, err := d.client.Deployments()
	if err != nil {
		d.fetchErr = err
		return
	}

	for _, resp := range resps {
		if resp.Name == d.name {
			d.fill(resp)
			return
		}
	}

	d.fetchErr = bosherr.Errorf("Expected to find deployment '%s'", d.name)
}

func (d *DeploymentImpl) fill(resp DeploymentResp) {
	d.fetched = true

	rels, err := newReleasesFromResps(resp.Releases, d.client)
	if err != nil {
		d.fetchErr = err
		return
	}

	stems, err := newStemcellsFromResps(resp.Stemcells, d.client)
	if err != nil {
		d.fetchErr = err
		return
	}

	d.releases = rels
	d.stemcells = stems
	d.cloudConfig = resp.CloudConfig
}

func (d DeploymentImpl) Manifest() (string, error) {
	resp, err := d.client.Deployment(d.name)
	if err != nil {
		return "", bosherr.WrapErrorf(err, "Fetching manifest")
	}

	return resp.Manifest, nil
}

func (d DeploymentImpl) FetchLogs(slug InstanceSlug, filters []string, agent bool) (LogsResult, error) {
	blobID, sha1, err := d.client.FetchLogs(d.name, slug.Name(), slug.IndexOrID(), filters, agent)
	if err != nil {
		return LogsResult{}, err
	}

	return LogsResult{BlobstoreID: blobID, SHA1: sha1}, nil
}

func (d DeploymentImpl) EnableResurrection(slug InstanceSlug, enabled bool) error {
	return d.client.EnableResurrection(d.name, slug.Name(), slug.IndexOrID(), enabled)
}

func (d DeploymentImpl) Start(slug AllOrPoolOrInstanceSlug) error {
	return d.changeJobState("started", slug, SkipDrain{}, false)
}

func (d DeploymentImpl) Stop(slug AllOrPoolOrInstanceSlug, hard bool, sd SkipDrain, force bool) error {
	if hard {
		return d.changeJobState("detached", slug, sd, force)
	}
	return d.changeJobState("stopped", slug, sd, force)
}

func (d DeploymentImpl) Restart(slug AllOrPoolOrInstanceSlug, sd SkipDrain, force bool) error {
	return d.changeJobState("restart", slug, sd, force)
}

func (d DeploymentImpl) Recreate(slug AllOrPoolOrInstanceSlug, sd SkipDrain, force bool) error {
	return d.changeJobState("recreate", slug, sd, force)
}

func (d DeploymentImpl) changeJobState(state string, slug AllOrPoolOrInstanceSlug, sd SkipDrain, force bool) error {
	manifest, err := d.Manifest()
	if err != nil {
		return err
	}

	return d.client.ChangeJobState(
		state, d.name, slug.Name(), slug.IndexOrID(), sd, force, []byte(manifest))
}

func (d DeploymentImpl) ExportRelease(release ReleaseSlug, os OSVersionSlug) (ExportReleaseResult, error) {
	resp, err := d.client.ExportRelease(d.name, release, os)
	if err != nil {
		return ExportReleaseResult{}, err
	}

	return ExportReleaseResult{BlobstoreID: resp.BlobstoreID, SHA1: resp.SHA1}, nil
}

func (d DeploymentImpl) Update(manifest []byte, recreate bool, sd SkipDrain) error {
	return d.client.UpdateDeployment(manifest, recreate, sd)
}

func (d DeploymentImpl) Delete(force bool) error {
	err := d.client.DeleteDeployment(d.name, force)
	if err != nil {
		resps, listErr := d.client.Deployments()
		if listErr != nil {
			return err
		}

		for _, resp := range resps {
			if resp.Name == d.name {
				return err
			}
		}
	}

	return nil
}

func (d DeploymentImpl) IsInProgress() (bool, error) {
	lockResps, err := d.client.Locks()
	if err != nil {
		return false, err
	}

	for _, r := range lockResps {
		if r.IsForDeployment(d.name) {
			return true, nil
		}
	}

	return false, nil
}

func (c Client) FetchLogs(deploymentName, job, indexOrID string, filters []string, agent bool) (string, string, error) {
	if len(deploymentName) == 0 {
		return "", "", bosherr.Error("Expected non-empty deployment name")
	}

	if len(job) == 0 {
		return "", "", bosherr.Error("Expected non-empty job name")
	}

	if len(indexOrID) == 0 {
		return "", "", bosherr.Error("Expected non-empty index or ID")
	}

	query := gourl.Values{}

	if len(filters) > 0 {
		query.Add("filters", strings.Join(filters, ","))
	}

	if agent {
		query.Add("type", "agent")
	} else {
		query.Add("type", "job")
	}

	path := fmt.Sprintf("/deployments/%s/jobs/%s/%s/logs?%s",
		deploymentName, job, indexOrID, query.Encode())

	taskID, _, err := c.taskClientRequest.GetResult(path)
	if err != nil {
		return "", "", bosherr.WrapErrorf(err, "Fetching logs")
	}

	taskResp, err := c.Task(taskID)
	if err != nil {
		return "", "", err
	}

	return taskResp.Result, "", nil
}

func (c Client) EnableResurrection(deploymentName, job, indexOrID string, enabled bool) error {
	if len(deploymentName) == 0 {
		return bosherr.Error("Expected non-empty deployment name")
	}

	if len(job) == 0 {
		return bosherr.Error("Expected non-empty job name")
	}

	if len(indexOrID) == 0 {
		return bosherr.Error("Expected non-empty index or ID")
	}

	path := fmt.Sprintf("/deployments/%s/jobs/%s/%s/resurrection",
		deploymentName, job, indexOrID)

	body := map[string]bool{"resurrection_paused": !enabled}

	reqBody, err := json.Marshal(body)
	if err != nil {
		return bosherr.WrapErrorf(err, "Marshaling request body")
	}

	setHeaders := func(req *http.Request) {
		req.Header.Add("Content-Type", "application/json")
	}

	_, _, err = c.clientRequest.RawPut(path, reqBody, setHeaders)
	if err != nil {
		msg := "Changing VM resurrection state for '%s/%s' in deployment '%s'"
		return bosherr.WrapErrorf(err, msg, job, indexOrID, deploymentName)
	}

	return nil
}

func (c Client) ChangeJobState(state, deploymentName, job, indexOrID string, sd SkipDrain, force bool, manifest []byte) error {
	if len(state) == 0 {
		return bosherr.Error("Expected non-empty job state")
	}

	if len(deploymentName) == 0 {
		return bosherr.Error("Expected non-empty deployment name")
	}

	// allows to have empty job and indexOrID

	query := gourl.Values{}

	query.Add("state", state)

	if len(sd.AsQueryValue()) > 0 {
		query.Add("skip_drain", sd.AsQueryValue())
	}

	if force {
		query.Add("force", "true")
	}

	path := fmt.Sprintf("/deployments/%s/jobs", deploymentName)

	if len(job) > 0 {
		path += "/" + job

		if len(indexOrID) > 0 {
			path += "/" + indexOrID
		}
	} else {
		path += "/*"
	}

	path += "?" + query.Encode()

	setHeaders := func(req *http.Request) {
		req.Header.Add("Content-Type", "text/yaml")
	}

	_, err := c.taskClientRequest.PutResult(path, manifest, setHeaders)
	if err != nil {
		return bosherr.WrapErrorf(err, "Changing state")
	}

	return nil
}

func (c Client) ExportRelease(deploymentName string, release ReleaseSlug, os OSVersionSlug) (ExportReleaseResp, error) {
	var resp ExportReleaseResp

	if len(deploymentName) == 0 {
		return resp, bosherr.Error("Expected non-empty deployment name")
	}

	if len(release.Name()) == 0 {
		return resp, bosherr.Error("Expected non-empty release name")
	}

	if len(release.Version()) == 0 {
		return resp, bosherr.Error("Expected non-empty release version")
	}

	if len(os.OS()) == 0 {
		return resp, bosherr.Error("Expected non-empty OS name")
	}

	if len(os.Version()) == 0 {
		return resp, bosherr.Error("Expected non-empty OS version")
	}

	path := "/releases/export"

	body := map[string]string{
		"deployment_name":  deploymentName,
		"release_name":     release.Name(),
		"release_version":  release.Version(),
		"stemcell_os":      os.OS(),
		"stemcell_version": os.Version(),
	}

	reqBody, err := json.Marshal(body)
	if err != nil {
		return resp, bosherr.WrapErrorf(err, "Marshaling request body")
	}

	setHeaders := func(req *http.Request) {
		req.Header.Add("Content-Type", "application/json")
	}

	resultBytes, err := c.taskClientRequest.PostResult(path, reqBody, setHeaders)
	if err != nil {
		return resp, bosherr.WrapErrorf(err, "Exporting release")
	}

	err = json.Unmarshal(resultBytes, &resp)
	if err != nil {
		return resp, bosherr.WrapErrorf(err, "Unmarshaling export release result")
	}

	return resp, nil
}

func (c Client) UpdateDeployment(manifest []byte, recreate bool, sd SkipDrain) error {
	query := gourl.Values{}

	if recreate {
		query.Add("recreate", "true")
	}

	if len(sd.AsQueryValue()) > 0 {
		query.Add("skip_drain", sd.AsQueryValue())
	}

	path := fmt.Sprintf("/deployments?%s", query.Encode())

	setHeaders := func(req *http.Request) {
		req.Header.Add("Content-Type", "text/yaml")
	}

	_, err := c.taskClientRequest.PostResult(path, manifest, setHeaders)
	if err != nil {
		return bosherr.WrapErrorf(err, "Updating deployment")
	}

	return nil
}

func (c Client) DeleteDeployment(deploymentName string, force bool) error {
	if len(deploymentName) == 0 {
		return bosherr.Error("Expected non-empty deployment name")
	}

	query := gourl.Values{}

	if force {
		query.Add("force", "true")
	}

	path := fmt.Sprintf("/deployments/%s?%s", deploymentName, query.Encode())

	_, err := c.taskClientRequest.DeleteResult(path)
	if err != nil {
		return bosherr.WrapErrorf(err, "Deleting deployment '%s'", deploymentName)
	}

	return nil
}

type VMResp struct {
	JobName  string `json:"job"`   // e.g. dummy1
	JobIndex int    `json:"index"` // e.g. 0,1,2

	AgentID string `json:"agent_id"` // e.g. 3b30123e-dfa6-4eff-abe6-63c2d5a88938
	CID     string // e.g. vm-ce10ae6a-6c31-413b-a134-7179f49e0bda
}

func (c Client) DeploymentVMs(deploymentName string) ([]VMResp, error) {
	if len(deploymentName) == 0 {
		return nil, bosherr.Error("Expected non-empty deployment name")
	}

	var vms []VMResp

	path := fmt.Sprintf("/deployments/%s/vms", deploymentName)

	err := c.clientRequest.Get(path, &vms)
	if err != nil {
		return vms, bosherr.WrapErrorf(err, "Listing deployment '%s' VMs", deploymentName)
	}

	return vms, nil
}