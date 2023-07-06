package chaos_experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/chaos_infrastructure"

	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb"
	dbChaosExperimentRun "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb/chaos_experiment_run"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/graph/model"
	store "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/data-store"
	dbChaosExperiment "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb/chaos_experiment"

	dbChaosInfra "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb/chaos_infrastructure"

	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/utils"

	"github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/ghodss/yaml"
	"github.com/google/uuid"
	chaosTypes "github.com/litmuschaos/chaos-operator/api/litmuschaos/v1alpha1"
	scheduleTypes "github.com/litmuschaos/chaos-scheduler/api/litmuschaos/v1alpha1"
	"go.mongodb.org/mongo-driver/bson"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type Service interface {
	ProcessExperiment(workflow *model.ChaosExperimentRequest, projectID string, revID string) (*model.ChaosExperimentRequest, *dbChaosExperiment.ChaosExperimentType, error)
	ProcessExperimentCreation(ctx context.Context, input *model.ChaosExperimentRequest, username string, projectID string, wfType *dbChaosExperiment.ChaosExperimentType, revisionID string, r *store.StateData) error
	ProcessExperimentUpdate(workflow *model.ChaosExperimentRequest, username string, wfType *dbChaosExperiment.ChaosExperimentType, revisionID string, updateRevision bool, projectID string, r *store.StateData) error
	ProcessExperimentDelete(query bson.D, workflow dbChaosExperiment.ChaosExperimentRequest, username string, r *store.StateData) error
	ProcessExperimentRunDelete(ctx context.Context, query bson.D, workflowRunID *string, experimentRun dbChaosExperimentRun.ChaosExperimentRun, workflow dbChaosExperiment.ChaosExperimentRequest, username string, r *store.StateData) error
	ProcessCompletedExperimentRun(execData ExecutionData, wfID string, runID string) (ExperimentRunMetrics, error)
}

// chaosWorkflowService is the implementation of the chaos workflow service
type chaosExperimentService struct {
	chaosExperimentOperator     *dbChaosExperiment.Operator
	chaosInfrastructureOperator *dbChaosInfra.Operator
}

// NewChaosExperimentService returns a new instance of the chaos workflow service
func NewChaosExperimentService(chaosWorkflowOperator *dbChaosExperiment.Operator, clusterOperator *dbChaosInfra.Operator) Service {
	return &chaosExperimentService{
		chaosExperimentOperator:     chaosWorkflowOperator,
		chaosInfrastructureOperator: clusterOperator,
	}
}

// ProcessExperiment takes the workflow and processes it as required
func (c *chaosExperimentService) ProcessExperiment(workflow *model.ChaosExperimentRequest, projectID string, revID string) (*model.ChaosExperimentRequest, *dbChaosExperiment.ChaosExperimentType, error) {
	// security check for chaos_infra access
	infra, err := c.chaosInfrastructureOperator.GetInfra(workflow.InfraID)
	if err != nil {
		return nil, nil, errors.New("failed to get infra details: " + err.Error())
	}

	if !infra.IsActive {
		return nil, nil, errors.New("experiment scheduling failed due to inactive infra")
	}

	if infra.ProjectID != projectID {
		return nil, nil, errors.New("ProjectID doesn't match with the chaos_infra identifiers")
	}

	wfType := dbChaosExperiment.NonCronExperiment
	var (
		workflowID = uuid.New().String()
		weights    = make(map[string]int)
		objMeta    unstructured.Unstructured
	)

	if len(workflow.Weightages) > 0 {
		for _, weight := range workflow.Weightages {
			weights[weight.FaultName] = weight.Weightage
		}
	}

	if workflow.ExperimentID == nil || (*workflow.ExperimentID) == "" {
		workflow.ExperimentID = &workflowID
	}

	err = json.Unmarshal([]byte(workflow.ExperimentManifest), &objMeta)
	if err != nil {
		return nil, nil, errors.New("failed to unmarshal workflow manifest1")
	}

	// workflow name in struct should match with actual workflow name
	if workflow.ExperimentName != objMeta.GetName() {
		return nil, nil, errors.New(objMeta.GetKind() + " name doesn't match")
	}

	switch strings.ToLower(objMeta.GetKind()) {
	case "workflow":
		{
			err = processExperimentManifest(workflow, weights, revID)
			if err != nil {
				return nil, nil, err
			}
		}
	case "cronworkflow":
		{
			wfType = dbChaosExperiment.CronExperiment
			err = processCronExperimentManifest(workflow, weights, revID)
			if err != nil {
				return nil, nil, err
			}
		}
	case "chaosengine":
		{
			wfType = dbChaosExperiment.ChaosEngine
			err = processChaosEngineManifest(workflow, weights, revID)
			if err != nil {
				return nil, nil, err
			}

		}
	case "chaosschedule":
		{
			wfType = dbChaosExperiment.ChaosEngine
			err = processChaosScheduleManifest(workflow, weights, revID)
			if err != nil {
				return nil, nil, err
			}
		}
	default:
		{
			return nil, nil, errors.New("not a valid object, only workflows/cron workflows/chaos engines supported")
		}
	}

	return workflow, &wfType, nil
}

// ProcessExperimentCreation creates new workflow entry and sends the workflow to the specific chaos_infra for execution
func (c *chaosExperimentService) ProcessExperimentCreation(ctx context.Context, input *model.ChaosExperimentRequest, username string, projectID string, wfType *dbChaosExperiment.ChaosExperimentType, revisionID string, r *store.StateData) error {
	var (
		weightages []*dbChaosExperiment.WeightagesInput
		revision   []dbChaosExperiment.ExperimentRevision
	)
	if input.Weightages != nil {
		//TODO: Once we make the new chaos terminology change in APIs, then we can we the copier instead of for loop
		for _, v := range input.Weightages {
			weightages = append(weightages, &dbChaosExperiment.WeightagesInput{
				FaultName: v.FaultName,
				Weightage: v.Weightage,
			})
		}
	}

	timeNow := time.Now().UnixMilli()

	revision = append(revision, dbChaosExperiment.ExperimentRevision{
		RevisionID:         revisionID,
		ExperimentManifest: input.ExperimentManifest,
		UpdatedAt:          timeNow,
		Weightages:         weightages,
	})

	newChaosExperiment := dbChaosExperiment.ChaosExperimentRequest{
		ExperimentID:       *input.ExperimentID,
		CronSyntax:         input.CronSyntax,
		ExperimentType:     *wfType,
		IsCustomExperiment: input.IsCustomExperiment,
		InfraID:            input.InfraID,
		ResourceDetails: mongodb.ResourceDetails{
			Name:        input.ExperimentName,
			Description: input.ExperimentDescription,
			Tags:        input.Tags,
		},
		ProjectID: projectID,
		Audit: mongodb.Audit{
			CreatedAt: timeNow,
			UpdatedAt: timeNow,
			IsRemoved: false,
			CreatedBy: username,
			UpdatedBy: username,
		},
		Revision:                   revision,
		RecentExperimentRunDetails: []dbChaosExperiment.ExperimentRunDetail{},
	}

	err := c.chaosExperimentOperator.InsertChaosExperiment(ctx, newChaosExperiment)
	if err != nil {
		return err
	}
	if r != nil {
		chaos_infrastructure.SendExperimentToSubscriber(projectID, input, &username, nil, "create", r)
	}
	return nil
}

// ProcessExperimentUpdate updates the workflow entry and sends update resource request to required agent
func (c *chaosExperimentService) ProcessExperimentUpdate(workflow *model.ChaosExperimentRequest, username string, wfType *dbChaosExperiment.ChaosExperimentType, revisionID string, updateRevision bool, projectID string, r *store.StateData) error {
	var (
		weightages  []*dbChaosExperiment.WeightagesInput
		workflowObj unstructured.Unstructured
	)

	if workflow.Weightages != nil {
		//TODO: Once we make the new chaos terminology change in APIs, then we can use the copier instead of for loop
		for _, v := range workflow.Weightages {
			weightages = append(weightages, &dbChaosExperiment.WeightagesInput{
				FaultName: v.FaultName,
				Weightage: v.Weightage,
			})
		}
	}

	workflowRevision := dbChaosExperiment.ExperimentRevision{
		RevisionID:         revisionID,
		ExperimentManifest: workflow.ExperimentManifest,
		UpdatedAt:          time.Now().UnixMilli(),
		Weightages:         weightages,
	}

	query := bson.D{
		{"experiment_id", workflow.ExperimentID},
		{"project_id", projectID},
	}

	update := bson.D{
		{"$set", bson.D{
			{"experiment_type", *wfType},
			{"cron_syntax", workflow.CronSyntax},
			{"name", workflow.ExperimentName},
			{"tags", workflow.Tags},
			{"infra_id", workflow.InfraID},
			{"description", workflow.ExperimentDescription},
			{"is_custom_experiment", workflow.IsCustomExperiment},
			{"updated_at", time.Now().UnixMilli()},
			{"updated_by", username},
		}},
		{"$push", bson.D{
			{"revision", workflowRevision},
		}},
	}

	// This case is required while disabling/enabling cron experiments
	if updateRevision {
		query = bson.D{
			{"experiment_id", workflow.ExperimentID},
			{"project_id", projectID},
			{"revision.revision_id", revisionID}}
		update = bson.D{
			{"$set", bson.D{
				{"updated_at", time.Now().UnixMilli()},
				{"updated_by", username},
				{"revision.$.updated_at", time.Now().UnixMilli()},
				{"revision.$.experiment_manifest", workflow.ExperimentManifest},
			}},
		}
	}

	err := c.chaosExperimentOperator.UpdateChaosExperiment(context.Background(), query, update)
	if err != nil {
		return err
	}

	err = json.Unmarshal([]byte(workflow.ExperimentManifest), &workflowObj)
	if err != nil {
		return errors.New("failed to unmarshal workflow manifest1")
	}

	if /* strings.ToLower(workflowObj.GetKind()) == "cronworkflow" */ r != nil {
		chaos_infrastructure.SendExperimentToSubscriber(projectID, workflow, &username, nil, "update", r)
	}
	return nil
}

// ProcessExperimentDelete deletes the workflow entry and sends delete resource request to required chaos_infra
func (c *chaosExperimentService) ProcessExperimentDelete(query bson.D, workflow dbChaosExperiment.ChaosExperimentRequest, username string, r *store.StateData) error {
	var (
		wc      = writeconcern.New(writeconcern.WMajority())
		rc      = readconcern.Snapshot()
		txnOpts = options.Transaction().SetWriteConcern(wc).SetReadConcern(rc)
		ctx     = context.Background()
	)

	session, err := mongodb.MgoClient.StartSession()
	if err != nil {
		return err
	}

	err = mongo.WithSession(ctx, session, func(sessionContext mongo.SessionContext) error {
		if err = session.StartTransaction(txnOpts); err != nil {
			return err
		}

		//Update chaosExperiments collection
		update := bson.D{
			{"$set", bson.D{
				{"is_removed", true},
				{"updated_by", username},
				{"updated_at", time.Now().UnixMilli()},
			}},
		}
		err = c.chaosExperimentOperator.UpdateChaosExperiment(sessionContext, query, update)
		if err != nil {
			return err
		}

		//Update chaosExperimentRuns collection
		err = dbChaosExperimentRun.UpdateExperimentRunsWithQuery(sessionContext, bson.D{{"experiment_id", workflow.ExperimentID}}, update)
		if err != nil {
			return err
		}
		if err = session.CommitTransaction(sessionContext); err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		if abortErr := session.AbortTransaction(ctx); abortErr != nil {
			return abortErr
		}
		return err
	}

	session.EndSession(ctx)
	if r != nil {
		chaos_infrastructure.SendExperimentToSubscriber(workflow.ProjectID, &model.ChaosExperimentRequest{
			InfraID: workflow.InfraID,
		}, &username, &workflow.ExperimentID, "workflow_delete", r)
	}

	return nil
}

// ProcessExperimentRunDelete deletes a workflow entry and updates the database
func (c *chaosExperimentService) ProcessExperimentRunDelete(ctx context.Context, query bson.D, workflowRunID *string, experimentRun dbChaosExperimentRun.ChaosExperimentRun, workflow dbChaosExperiment.ChaosExperimentRequest, username string, r *store.StateData) error {
	update := bson.D{
		{"$set", bson.D{
			{"is_removed", experimentRun.IsRemoved},
			{"updated_at", time.Now().UnixMilli()},
			{"updated_by", username},
		}},
	}

	err := dbChaosExperimentRun.UpdateExperimentRunWithQuery(ctx, query, update)
	if err != nil {
		return err
	}
	if r != nil {
		chaos_infrastructure.SendExperimentToSubscriber(experimentRun.ProjectID, &model.ChaosExperimentRequest{
			InfraID: workflow.InfraID,
		}, &username, workflowRunID, "workflow_run_delete", r)
	}

	return nil
}

// ProcessCompletedExperimentRun calculates the Resiliency Score and returns the updated ExecutionData
func (c *chaosExperimentService) ProcessCompletedExperimentRun(execData ExecutionData, wfID string, runID string) (ExperimentRunMetrics, error) {
	var weightSum, totalTestResult = 0, 0
	var result ExperimentRunMetrics
	weightMap := map[string]int{}

	chaosExperiments, err := c.chaosExperimentOperator.GetExperiment(context.TODO(), bson.D{
		{"experiment_id", wfID},
	})
	if err != nil {
		return result, fmt.Errorf("failed to get experiment from db on complete, error: %w", err)
	}
	for _, rev := range chaosExperiments.Revision {
		if rev.RevisionID == execData.RevisionID {
			for _, weights := range rev.Weightages {
				weightMap[weights.FaultName] = weights.Weightage
				// Total weight calculated for all experiments
				weightSum = weightSum + weights.Weightage
			}
		}
	}

	result.TotalExperiments = len(weightMap)

	for _, value := range execData.Nodes {
		if value.Type == "ChaosEngine" {
			experimentName := ""
			if value.ChaosExp == nil {
				continue
			}

			for expName := range weightMap {
				if strings.Contains(value.ChaosExp.EngineName, expName) {
					experimentName = expName
				}
			}
			weight, ok := weightMap[experimentName]
			// probeSuccessPercentage will be included only if chaosData is present
			if ok {
				x, _ := strconv.Atoi(value.ChaosExp.ProbeSuccessPercentage)
				totalTestResult += weight * x
			}
			if value.ChaosExp.FaultVerdict == "Pass" {
				result.FaultsPassed += 1
			}
			if value.ChaosExp.FaultVerdict == "Fail" {
				result.FaultsFailed += 1
			}
			if value.ChaosExp.FaultVerdict == "Awaited" {
				result.FaultsAwaited += 1
			}
			if value.ChaosExp.FaultVerdict == "Stopped" {
				result.FaultsStopped += 1
			}
			if value.ChaosExp.FaultVerdict == "N/A" || value.ChaosExp.FaultVerdict == "" {
				result.FaultsNA += 1
			}
		}
	}
	if weightSum != 0 {
		result.ResiliencyScore = utils.Truncate(float64(totalTestResult) / float64(weightSum))
	}

	return result, nil
}

func processExperimentManifest(workflow *model.ChaosExperimentRequest, weights map[string]int, revID string) error {
	var (
		newWeights       []*model.WeightagesInput
		workflowManifest v1alpha1.Workflow
	)

	err := json.Unmarshal([]byte(workflow.ExperimentManifest), &workflowManifest)
	if err != nil {
		return errors.New("failed to unmarshal workflow manifest")
	}

	if workflowManifest.Labels == nil {
		workflowManifest.Labels = map[string]string{
			"workflow_id": *workflow.ExperimentID,
			"infra_id":    workflow.InfraID,
			"workflows.argoproj.io/controller-instanceid": workflow.InfraID,
			"revision_id": revID,
		}
	} else {
		workflowManifest.Labels["workflow_id"] = *workflow.ExperimentID
		workflowManifest.Labels["infra_id"] = workflow.InfraID
		workflowManifest.Labels["workflows.argoproj.io/controller-instanceid"] = workflow.InfraID
		workflowManifest.Labels["revision_id"] = revID
	}

	for i, template := range workflowManifest.Spec.Templates {
		artifact := template.Inputs.Artifacts
		if len(artifact) > 0 {
			if artifact[0].Raw == nil {
				continue
			}
			var data = artifact[0].Raw.Data
			if len(data) > 0 {
				// This replacement is required because chaos engine yaml have a syntax template. example:{{ workflow.parameters.adminModeNamespace }}
				// And it is not able the unmarshal the yamlstring to chaos engine struct
				data = strings.ReplaceAll(data, "{{", "")
				data = strings.ReplaceAll(data, "}}", "")

				var meta chaosTypes.ChaosEngine
				err := yaml.Unmarshal([]byte(data), &meta)
				if err != nil {
					return errors.New("failed to unmarshal chaosengine")
				}

				if strings.ToLower(meta.Kind) == "chaosengine" {
					var exprname string
					if len(meta.Spec.Experiments) > 0 {
						exprname = meta.GenerateName
						if len(exprname) == 0 {
							return errors.New("empty chaos experiment name")
						}
					} else {
						return errors.New("no experiments specified in chaosengine - " + meta.Name)
					}

					if val, ok := weights[exprname]; ok {
						workflowManifest.Spec.Templates[i].Metadata.Labels = map[string]string{
							"weight": strconv.Itoa(val),
						}
					} else if val, ok := workflowManifest.Spec.Templates[i].Metadata.Labels["weight"]; ok {
						intVal, err := strconv.Atoi(val)
						if err != nil {
							return errors.New("failed to convert")
						}
						newWeights = append(newWeights, &model.WeightagesInput{
							FaultName: exprname,
							Weightage: intVal,
						})
					} else {
						newWeights = append(newWeights, &model.WeightagesInput{
							FaultName: exprname,
							Weightage: 10,
						})

						workflowManifest.Spec.Templates[i].Metadata.Labels = map[string]string{
							"weight": "10",
						}
					}
				}
			}
		}
	}

	workflow.Weightages = append(workflow.Weightages, newWeights...)
	out, err := json.Marshal(workflowManifest)
	if err != nil {
		return err
	}

	workflow.ExperimentManifest = string(out)
	return nil
}

func processCronExperimentManifest(workflow *model.ChaosExperimentRequest, weights map[string]int, revID string) error {
	var (
		newWeights             []*model.WeightagesInput
		cronExperimentManifest v1alpha1.CronWorkflow
	)

	err := json.Unmarshal([]byte(workflow.ExperimentManifest), &cronExperimentManifest)
	if err != nil {
		return errors.New("failed to unmarshal workflow manifest")
	}

	if cronExperimentManifest.Labels == nil {
		cronExperimentManifest.Labels = map[string]string{
			"workflow_id": *workflow.ExperimentID,
			"infra_id":    workflow.InfraID,
			"workflows.argoproj.io/controller-instanceid": workflow.InfraID,
			"revision_id": revID,
		}
	} else {
		cronExperimentManifest.Labels["workflow_id"] = *workflow.ExperimentID
		cronExperimentManifest.Labels["infra_id"] = workflow.InfraID
		cronExperimentManifest.Labels["workflows.argoproj.io/controller-instanceid"] = workflow.InfraID
		cronExperimentManifest.Labels["revision_id"] = revID
	}

	if cronExperimentManifest.Spec.WorkflowMetadata == nil {
		cronExperimentManifest.Spec.WorkflowMetadata = &v1.ObjectMeta{
			Labels: map[string]string{
				"workflow_id": *workflow.ExperimentID,
				"infra_id":    workflow.InfraID,
				"workflows.argoproj.io/controller-instanceid": workflow.InfraID,
				"revision_id": revID,
			},
		}
	} else {
		if cronExperimentManifest.Spec.WorkflowMetadata.Labels == nil {
			cronExperimentManifest.Spec.WorkflowMetadata.Labels = map[string]string{
				"workflow_id": *workflow.ExperimentID,
				"infra_id":    workflow.InfraID,
				"workflows.argoproj.io/controller-instanceid": workflow.InfraID,
				"revision_id": revID,
			}
		} else {
			cronExperimentManifest.Spec.WorkflowMetadata.Labels["workflow_id"] = *workflow.ExperimentID
			cronExperimentManifest.Spec.WorkflowMetadata.Labels["infra_id"] = workflow.InfraID
			cronExperimentManifest.Spec.WorkflowMetadata.Labels["workflows.argoproj.io/controller-instanceid"] = workflow.InfraID
			cronExperimentManifest.Spec.WorkflowMetadata.Labels["revision_id"] = revID
		}
	}

	for i, template := range cronExperimentManifest.Spec.WorkflowSpec.Templates {

		artifact := template.Inputs.Artifacts
		if len(artifact) > 0 {
			if artifact[0].Raw == nil {
				continue
			}
			var data = artifact[0].Raw.Data
			if len(data) > 0 {
				// This replacement is required because chaos engine yaml have a syntax template. example:{{ workflow.parameters.adminModeNamespace }}
				// And it is not able the unmarshal the yamlstring to chaos engine struct
				data = strings.ReplaceAll(data, "{{", "")
				data = strings.ReplaceAll(data, "}}", "")

				var meta chaosTypes.ChaosEngine
				err = yaml.Unmarshal([]byte(data), &meta)
				if err != nil {
					return errors.New("failed to unmarshal chaosengine")
				}

				if strings.ToLower(meta.Kind) == "chaosengine" {
					var exprname string
					if len(meta.Spec.Experiments) > 0 {
						exprname = meta.GenerateName
						if len(exprname) == 0 {
							return errors.New("empty chaos experiment name")
						}
					} else {
						return errors.New("no experiments specified in chaosengine - " + meta.Name)
					}
					if val, ok := weights[exprname]; ok {
						cronExperimentManifest.Spec.WorkflowSpec.Templates[i].Metadata.Labels = map[string]string{
							"weight": strconv.Itoa(val),
						}
					} else if val, ok := cronExperimentManifest.Spec.WorkflowSpec.Templates[i].Metadata.Labels["weight"]; ok {
						intVal, err := strconv.Atoi(val)
						if err != nil {
							return errors.New("failed to convert")
						}

						newWeights = append(newWeights, &model.WeightagesInput{
							FaultName: exprname,
							Weightage: intVal,
						})
					} else {
						newWeights = append(newWeights, &model.WeightagesInput{
							FaultName: exprname,
							Weightage: 10,
						})
						cronExperimentManifest.Spec.WorkflowSpec.Templates[i].Metadata.Labels = map[string]string{
							"weight": "10",
						}
					}
				}
			}
		}
	}

	workflow.Weightages = append(workflow.Weightages, newWeights...)
	out, err := json.Marshal(cronExperimentManifest)
	if err != nil {
		return err
	}
	workflow.ExperimentManifest = string(out)
	workflow.CronSyntax = cronExperimentManifest.Spec.Schedule
	return nil
}

func processChaosEngineManifest(workflow *model.ChaosExperimentRequest, weights map[string]int, revID string) error {
	var (
		newWeights       []*model.WeightagesInput
		workflowManifest chaosTypes.ChaosEngine
	)

	err := json.Unmarshal([]byte(workflow.ExperimentManifest), &workflowManifest)
	if err != nil {
		return errors.New("failed to unmarshal workflow manifest")
	}

	if workflowManifest.Labels == nil {
		workflowManifest.Labels = map[string]string{
			"workflow_id": *workflow.ExperimentID,
			"infra_id":    workflow.InfraID,
			"type":        "standalone_workflow",
			"revision_id": revID,
		}
	} else {
		workflowManifest.Labels["workflow_id"] = *workflow.ExperimentID
		workflowManifest.Labels["infra_id"] = workflow.InfraID
		workflowManifest.Labels["type"] = "standalone_workflow"
		workflowManifest.Labels["revision_id"] = revID
	}

	if len(workflowManifest.Spec.Experiments) == 0 {
		return errors.New("no experiments specified in chaosengine - " + workflowManifest.Name)
	}
	exprName := workflowManifest.Spec.Experiments[0].Name
	if len(exprName) == 0 {
		return errors.New("empty chaos experiment name")
	}
	if val, ok := weights[exprName]; ok {
		workflowManifest.Labels["weight"] = strconv.Itoa(val)
	} else if val, ok := workflowManifest.Labels["weight"]; ok {
		intVal, err := strconv.Atoi(val)
		if err != nil {
			return errors.New("failed to convert")
		}
		newWeights = append(newWeights, &model.WeightagesInput{
			FaultName: exprName,
			Weightage: intVal,
		})
	} else {
		newWeights = append(newWeights, &model.WeightagesInput{
			FaultName: exprName,
			Weightage: 10,
		})
		workflowManifest.Labels["weight"] = "10"
	}
	workflow.Weightages = append(workflow.Weightages, newWeights...)
	out, err := json.Marshal(workflowManifest)
	if err != nil {
		return err
	}

	workflow.ExperimentManifest = string(out)
	return nil
}

func processChaosScheduleManifest(workflow *model.ChaosExperimentRequest, weights map[string]int, revID string) error {
	var (
		newWeights       []*model.WeightagesInput
		workflowManifest scheduleTypes.ChaosSchedule
	)
	err := json.Unmarshal([]byte(workflow.ExperimentManifest), &workflowManifest)
	if err != nil {
		return errors.New("failed to unmarshal workflow manifest")
	}

	if workflowManifest.Labels == nil {
		workflowManifest.Labels = map[string]string{
			"workflow_id": *workflow.ExperimentID,
			"infra_id":    workflow.InfraID,
			"type":        "standalone_workflow",
			"revision_id": revID,
		}
	} else {
		workflowManifest.Labels["workflow_id"] = *workflow.ExperimentID
		workflowManifest.Labels["infra_id"] = workflow.InfraID
		workflowManifest.Labels["type"] = "standalone_workflow"
		workflowManifest.Labels["revision_id"] = revID
	}
	if len(workflowManifest.Spec.EngineTemplateSpec.Experiments) == 0 {
		return errors.New("no experiments specified in chaos engine - " + workflowManifest.Name)
	}

	exprName := workflowManifest.Spec.EngineTemplateSpec.Experiments[0].Name
	if len(exprName) == 0 {
		return errors.New("empty chaos experiment name")
	}

	if val, ok := weights[exprName]; ok {
		workflowManifest.Labels["weight"] = strconv.Itoa(val)
	} else if val, ok := workflowManifest.Labels["weight"]; ok {
		intVal, err := strconv.Atoi(val)
		if err != nil {
			return errors.New("failed to convert")
		}
		newWeights = append(newWeights, &model.WeightagesInput{
			FaultName: exprName,
			Weightage: intVal,
		})
	} else {
		newWeights = append(newWeights, &model.WeightagesInput{
			FaultName: exprName,
			Weightage: 10,
		})
		workflowManifest.Labels["weight"] = "10"
	}
	workflow.Weightages = append(workflow.Weightages, newWeights...)
	out, err := json.Marshal(workflowManifest)
	if err != nil {
		return err
	}

	workflow.ExperimentManifest = string(out)
	return nil
}
