package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"path"

	"github.com/go-resty/resty/v2"
	"github.com/julienschmidt/httprouter"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

const (
	//pulumiURL = "https://api.pulumi.com/api"
	pulumiURL = "https://api.pat.pulumi-dev.io/api"
)

type DeploymentSettings struct {
	SourceContext    *sourceContext    `json:"sourceContext,omitempty"`
	OperationContext *operationContext `json:"operationContext,omitempty"`
	GitHub           *gitHubContext    `json:"gitHub,omitempty"`
}

type sourceContext struct {
	Git gitContext
}

type gitContext struct {
	Branch  string `json:"branch,omitempty"`
	RepoDir string `json:"repoDir,omitempty"`
}

type operationContext struct {
	Environment map[string]string `json:"environmentVariables,omitempty"`
	OIDC        *oidcContext      `json:"oidc,omitempty"`
}

type oidcContext struct {
	AWS *awsOIDCContext `json:"aws,omitempty"`
}

type awsOIDCContext struct {
	RoleARN     string `json:"roleArn,omitempty"`
	SessionName string `json:"sessionName,omitempty"`
}

type gitHubContext struct {
	Repository          string   `json:"repository,omitempty"`
	Paths               []string `json:"paths,omitempty"`
	DeployCommits       bool     `json:"deployCommits,omitempty"`
	PreviewPullRequests bool     `json:"previewPullRequests,omitempty"`
}

type createDeploymentRequest struct {
	DeploymentSettings

	InheritSettings bool   `json:"inheritSettings"`
	Operation       string `json:"operation"`
}

type createStackRequest struct {
	StackName string `json:"stackName"`
}

type operationStatus struct {
	Kind    string `json:"kind"`
	Author  string `json:"author"`
	Started int64  `json:"started"`
}

type getStackResponse struct {
	CurrentOperation *operationStatus `json:"currentOperation,omitempty"`
}

type organizationSummary struct {
	GitHubLogin string `json:"githubLogin"`
}

type getUserResponse struct {
	Organizations []organizationSummary `json:"organizations"`
}

type createSiteRequest struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

type updateSiteRequest struct {
	Content string `json:"content"`
}

type getSiteResponse struct {
	ID     string `json:"id"`
	URL    string `json:"url,omitempty"`
	Status string `json:"status,omitempty"`
}

func internalServerError(w http.ResponseWriter, err error) {
	w.WriteHeader(500)
	fmt.Fprintf(w, "Internal Server Error")
	log.Printf("Internal Server Error: %v", err)
}

type siteServer struct {
	client *resty.Client

	repository string
	branch     string
	dir        string

	roleARN     string
	sessionName string

	apiToken string
	org      string
	project  string
}

func (s *siteServer) updateStack(ctx context.Context, stack, content string) error {
	resp, err := s.client.R().
		SetContext(ctx).
		SetBody(createDeploymentRequest{
			DeploymentSettings: DeploymentSettings{
				OperationContext: &operationContext{
					Environment: map[string]string{
						"SITE_CONTENT": content,
					},
				},
			},
			InheritSettings: true,
			Operation:       "update",
		}).
		SetHeader("Authorization", "token "+s.apiToken).
		SetHeader("Accept", "application/json").
		Post(pulumiURL + path.Join("/preview", s.org, s.project, stack, "deployments"))
	if err != nil {
		return err
	}
	if resp.StatusCode() != http.StatusAccepted {
		return errors.New(string(resp.Body()))
	}
	return nil
}

func (s *siteServer) create(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var create createSiteRequest
	if err := json.NewDecoder(r.Body).Decode(&create); err != nil {
		w.WriteHeader(400)
		fmt.Fprintf(w, "failed to parse create request")
		return
	}

	stack := create.ID

	resp, err := s.client.R().
		SetContext(r.Context()).
		SetBody(createStackRequest{StackName: stack}).
		SetHeader("Authorization", "token "+s.apiToken).
		SetHeader("Accept", "application/json").
		Post(pulumiURL + path.Join("/stacks", s.org, s.project))
	if err != nil {
		internalServerError(w, fmt.Errorf("creating stack: %w", err))
		return
	}
	if resp.StatusCode() != 200 && resp.StatusCode() != 409 {
		internalServerError(w, fmt.Errorf("creating stack: %s", string(resp.Body())))
		return
	}
	log.Printf("created stack '%s/%s/%s'", s.org, s.project, stack)

	var paths []string
	if s.dir != "" {
		paths = []string{s.dir + "/**"}
	}

	settings := DeploymentSettings{
		SourceContext: &sourceContext{
			Git: gitContext{
				Branch:  s.branch,
				RepoDir: s.dir,
			},
		},
		OperationContext: &operationContext{
			Environment: map[string]string{
				"AWS_REGION": "us-west-2",
			},
			OIDC: &oidcContext{
				AWS: &awsOIDCContext{
					RoleARN:     s.roleARN,
					SessionName: s.sessionName,
				},
			},
		},
		GitHub: &gitHubContext{
			Repository:          s.repository,
			Paths:               paths,
			DeployCommits:       true,
			PreviewPullRequests: false,
		},
	}
	resp, err = s.client.R().
		SetContext(r.Context()).
		SetBody(settings).
		SetHeader("Authorization", "token "+s.apiToken).
		SetHeader("Accept", "application/json").
		Post(pulumiURL + path.Join("/preview", s.org, s.project, stack, "deployment", "settings"))
	if err != nil {
		internalServerError(w, fmt.Errorf("configuring deployment: %w", err))
		return
	}
	if resp.StatusCode() != 200 {
		internalServerError(w, fmt.Errorf("configuring deployment: %s", string(resp.Body())))
		return
	}

	if err := s.updateStack(r.Context(), stack, create.Content); err != nil {
		internalServerError(w, fmt.Errorf("starting deployment: %w", err))
		return
	}

	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(&getSiteResponse{ID: stack}); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func (s *siteServer) get(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id := params.ByName("id")

	operation, err := func() (*operationStatus, error) {
		resp, err := s.client.R().
			SetContext(r.Context()).
			SetHeader("Authorization", "token "+s.apiToken).
			SetHeader("Accept", "application/json").
			SetDoNotParseResponse(true).
			Get(pulumiURL + path.Join("/stacks", s.org, s.project, id))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode() != 200 {
			return nil, errors.New(string(resp.Body()))
		}
		defer resp.RawBody().Close()

		var respBody getStackResponse
		if err = json.NewDecoder(resp.RawBody()).Decode(&respBody); err != nil {
			return nil, err
		}
		return respBody.CurrentOperation, nil
	}()
	if err != nil {
		internalServerError(w, fmt.Errorf("getting stack: %w", err))
		return
	}
	status := "IDLE"
	if operation != nil {
		if operation.Kind == "destroy" {
			status = "DELETING"
		} else {
			status = "UPDATING"
		}
	}

	outputs, err := func() (map[string]interface{}, error) {
		resp, err := s.client.R().
			SetContext(r.Context()).
			SetHeader("Authorization", "token "+s.apiToken).
			SetHeader("Accept", "application/json").
			SetDoNotParseResponse(true).
			Get(pulumiURL + path.Join("/stacks", s.org, s.project, id, "export"))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode() != 200 {
			return nil, errors.New(string(resp.Body()))
		}
		defer resp.RawBody().Close()

		var respBody apitype.UntypedDeployment
		if err = json.NewDecoder(resp.RawBody()).Decode(&respBody); err != nil {
			return nil, err
		}
		if respBody.Version != apitype.DeploymentSchemaVersionCurrent {
			return nil, nil
		}
		var stack apitype.DeploymentV3
		if err = json.Unmarshal([]byte(respBody.Deployment), &stack); err != nil {
			return nil, fmt.Errorf("unmarshaling deployment: %w", err)
		}
		var stackResource *apitype.ResourceV3
		for _, r := range stack.Resources {
			if r.Type == "pulumi:pulumi:Stack" {
				stackResource = &r
				break
			}
		}
		if stackResource == nil {
			return nil, nil
		}
		return stackResource.Outputs, nil
	}()
	if err != nil {
		internalServerError(w, fmt.Errorf("getting stack outputs: %w", err))
		return
	}
	url, _ := outputs["websiteUrl"].(string)

	resp := getSiteResponse{
		ID:     id,
		URL:    url,
		Status: status,
	}
	if err = json.NewEncoder(w).Encode(&resp); err != nil {
		log.Printf("encoding response: %v", err)
	}
}

func (s *siteServer) update(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id := params.ByName("id")

	var update updateSiteRequest
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		w.WriteHeader(400)
		fmt.Fprintf(w, "failed to parse update request")
		return
	}

	if err := s.updateStack(r.Context(), id, update.Content); err != nil {
		internalServerError(w, fmt.Errorf("starting deployment: %w", err))
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

func (s *siteServer) delete(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id := params.ByName("id")

	resp, err := s.client.R().
		SetContext(r.Context()).
		SetBody(createDeploymentRequest{
			InheritSettings: true,
			Operation:       "destroy",
		}).
		SetHeader("Authorization", "token "+s.apiToken).
		SetHeader("Accept", "application/json").
		Post(pulumiURL + path.Join("/preview", s.org, s.project, id, "deployments"))
	if err != nil {
		internalServerError(w, fmt.Errorf("starting deployment: %w", err))
		return
	}
	if resp.StatusCode() != http.StatusAccepted {
		internalServerError(w, fmt.Errorf("starting deployment: %s", string(resp.Body())))
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func main() {
	repository := flag.String("repo", "", "the GitHub repository that contains the site's Pulumi program")
	branch := flag.String("branch", "main", "the git branch that contains the site's Pulumi program")
	dir := flag.String("dir", "", "the subdirectory of the git repository that contains the site's Pulumi program")
	roleARN := flag.String("role-arn", "", "the AWS IAM Role ARN to use for OIDC integration")
	sessionName := flag.String("session-name", "site-deploy", "the session name to use for AWS OIDC integration")
	apiToken := flag.String("token", "", "the Pulumi API token to use")
	org := flag.String("org", "", "the Pulumi organization to use")
	project := flag.String("project", "", "the Pulumi project to deploy")
	addr := flag.String("addr", ":8080", "the address to listen on")
	flag.Parse()

	if *repository == "" {
		log.Fatal("the -repo flag is required")
	}
	if *roleARN == "" {
		log.Fatal("the -role-arn flag is required")
	}
	if *apiToken == "" {
		log.Fatal("the -token flag is required")
	}
	if *project == "" {
		log.Fatal("the -project flag is required")
	}

	client := resty.New()

	if *org == "" {
		defaultOrg, err := func() (string, error) {
			resp, err := client.R().
				SetHeader("Authorization", "token "+*apiToken).
				SetHeader("Accept", "application/json").
				SetDoNotParseResponse(true).
				Get(pulumiURL + "/user")
			if err != nil {
				return "", err
			}
			if resp.StatusCode() != 200 {
				return "", fmt.Errorf("%v: %v", resp.StatusCode(), (string(resp.Body())))
			}
			defer resp.RawBody().Close()

			var body getUserResponse
			if err := json.NewDecoder(resp.RawBody()).Decode(&body); err != nil {
				return "", fmt.Errorf("decoding response: %w", err)
			}
			return body.Organizations[0].GitHubLogin, nil
		}()
		if err != nil {
			log.Fatalf("getting default organization: %v", err)
		}
		*org = defaultOrg
	}

	server := &siteServer{
		client:      client,
		repository:  *repository,
		branch:      *branch,
		dir:         *dir,
		roleARN:     *roleARN,
		sessionName: *sessionName,
		apiToken:    *apiToken,
		org:         *org,
		project:     *project,
	}
	router := httprouter.New()
	router.POST("/sites", server.create)
	router.GET("/sites/:id", server.get)
	router.POST("/sites/:id", server.update)
	router.DELETE("/sites/:id", server.delete)

	http.ListenAndServe(*addr, router)
}
