package graph

import (
	"context"

	"github.com/99designs/gqlgen/graphql"
	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/graph/generated"
	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/authorization"
	chaos_experiment2 "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/chaos_experiment"
	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/chaos_experiment/handler"
	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/chaos_infrastructure"
	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/chaoshub"
	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb"
	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb/chaos_experiment"
	dbSchemaChaosHub "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb/chaos_hub"
	dbChaosInfra "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb/chaos_infrastructure"
	gitops2 "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb/gitops"
	image_registry2 "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/database/mongodb/image_registry"
	gitops3 "github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/gitops"
	"github.com/litmuschaos/litmus/chaoscenter/graphql/server/pkg/image_registry"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

type Resolver struct {
	chaosHubService            chaoshub.Service
	imageRegistryService       image_registry.Service
	chaosInfrastructureService chaos_infrastructure.Service
	chaosExperimentService     chaos_experiment2.Service
	chaosExperimentHandler     handler.ChaosExperimentHandler
	gitopsService              gitops3.Service
}

func NewConfig(mongodbOperator mongodb.MongoOperator) generated.Config {
	//operator
	chaosHubOperator := dbSchemaChaosHub.NewChaosHubOperator(mongodbOperator)
	chaosInfraOperator := dbChaosInfra.NewInfrastructureOperator(mongodbOperator)
	chaosExperimentOperator := chaos_experiment.NewChaosExperimentOperator(mongodbOperator)

	gitopsOperator := gitops2.NewGitOpsOperator(mongodbOperator)
	imageRegistryOperator := image_registry2.NewImageRegistryOperator(mongodbOperator)

	//service
	chaosHubService := chaoshub.NewService(chaosHubOperator)
	chaosInfrastructureService := chaos_infrastructure.NewChaosInfrastructureService(chaosInfraOperator)
	chaosExperimentService := chaos_experiment2.NewChaosExperimentService(chaosExperimentOperator, chaosInfraOperator)
	gitOpsService := gitops3.NewGitOpsService(gitopsOperator, chaosExperimentService, *chaosExperimentOperator)
	imageRegistryService := image_registry.NewImageRegistryService(imageRegistryOperator)

	//handler
	chaosExperimentHandler := handler.NewChaosExperimentHandler(chaosExperimentService, chaosInfrastructureService, gitOpsService, chaosExperimentOperator, mongodbOperator)

	config := generated.Config{
		Resolvers: &Resolver{
			chaosHubService:            chaosHubService,
			chaosInfrastructureService: chaosInfrastructureService,
			chaosExperimentService:     chaosExperimentService,
			imageRegistryService:       imageRegistryService,
			gitopsService:              gitOpsService,
			chaosExperimentHandler:     *chaosExperimentHandler,
		}}

	config.Directives.Authorized = func(ctx context.Context, obj interface{}, next graphql.Resolver) (interface{}, error) {
		token := ctx.Value(authorization.AuthKey).(string)
		user, err := authorization.UserValidateJWT(token)
		if err != nil {
			return nil, err
		}
		newCtx := context.WithValue(ctx, authorization.UserClaim, user)
		return next(newCtx)
	}

	return config
}
