// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT license.

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/aks-engine/pkg/api"
	"github.com/Azure/aks-engine/pkg/armhelpers"
	"github.com/Azure/aks-engine/pkg/engine"
	"github.com/Azure/aks-engine/pkg/engine/transform"
	"github.com/Azure/aks-engine/pkg/helpers"
	"github.com/Azure/aks-engine/pkg/i18n"
	"github.com/leonelquinteros/gotext"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	prefixed "github.com/x-cray/logrus-prefixed-formatter"
	v1 "k8s.io/api/core/v1"
)

type generateAddPoolCmd struct {
	authArgs

	// user input
	apiModelPath      string
	resourceGroupName string
	agentPoolPath     string
	location          string
	noPrettyPrint     bool
	outputDirectory   string // can be auto-determined from clusterDefinition

	// derived
	containerService *api.ContainerService
	apiVersion       string
	agentPool        *api.AgentPoolProfile
	client           armhelpers.AKSEngineClient
	locale           *gotext.Locale
	nameSuffix       string
	agentPoolIndex   int
	logger           *log.Entry
	apiserverURL     string
	kubeconfig       string
	nodes            []v1.Node
}

const (
	generateAddPoolName             = "generate-addpool"
	generateAddPoolShortDescription = "Generate template files to add an agent pool to an existing Kubernetes cluster"
	generateAddPoolLongDescription  = "Generate template files to add an agent pool to an existing Kubernetes cluster by referencing a new agent pool spec"
	//	apiModelFilename        = "apimodel.json"
	// agentPoolFileName = "agentpool.json"
)

// newgenerateAddPoolCmd run a command to add an agent pool to a Kubernetes cluster
func newGenerateAddPoolCmd() *cobra.Command {
	apc := generateAddPoolCmd{}

	generateAddPoolCmd := &cobra.Command{
		Use:   generateAddPoolName,
		Short: generateAddPoolShortDescription,
		Long:  generateAddPoolLongDescription,
		RunE:  apc.run,
	}

	f := generateAddPoolCmd.Flags()
	f.StringVarP(&apc.location, "location", "l", "", "location the cluster is deployed in")
	f.StringVarP(&apc.resourceGroupName, "resource-group", "g", "", "the resource group where the cluster is deployed")
	f.StringVarP(&apc.apiModelPath, "api-model", "m", "", "path to the generated apimodel.json file")
	f.StringVarP(&apc.agentPoolPath, "agent-pool", "p", "", "path to the generated agentpool.json file")
	f.BoolVar(&apc.noPrettyPrint, "no-pretty-print", false, "skip pretty printing the output")
	f.StringVarP(&apc.outputDirectory, "output-directory", "o", "", "output directory (derived from FQDN if absent)")

	addAuthFlags(&apc.authArgs, f)

	return generateAddPoolCmd
}

func (apc *generateAddPoolCmd) validate(cmd *cobra.Command) error {
	log.Debugln("validating generate-addpool command line arguments...")
	var err error

	apc.locale, err = i18n.LoadTranslations()
	if err != nil {
		return errors.Wrap(err, "error loading translation files")
	}

	if apc.resourceGroupName == "" {
		cmd.Usage()
		return errors.New("--resource-group must be specified")
	}

	if apc.location == "" {
		cmd.Usage()
		return errors.New("--location must be specified")
	}

	apc.location = helpers.NormalizeAzureRegion(apc.location)

	if apc.apiModelPath == "" {
		cmd.Usage()
		return errors.New("--api-model must be specified")
	}

	if apc.agentPoolPath == "" {
		cmd.Usage()
		return errors.New("--agentpool must be specified")
	}
	return nil
}

func (apc *generateAddPoolCmd) load() error {
	logger := log.New()
	logger.Formatter = new(prefixed.TextFormatter)
	apc.logger = log.NewEntry(log.New())
	var err error

	ctx, cancel := context.WithTimeout(context.Background(), armhelpers.DefaultARMOperationTimeout)
	defer cancel()

	if _, err = os.Stat(apc.apiModelPath); os.IsNotExist(err) {
		return errors.Errorf("specified api model does not exist (%s)", apc.apiModelPath)
	}

	apiloader := &api.Apiloader{
		Translator: &i18n.Translator{
			Locale: apc.locale,
		},
	}
	apc.containerService, apc.apiVersion, err = apiloader.LoadContainerServiceFromFile(apc.apiModelPath, true, true, nil)
	if err != nil {
		return errors.Wrap(err, "error parsing the api model")
	}

	if _, err = os.Stat(apc.agentPoolPath); os.IsNotExist(err) {
		return errors.Errorf("specified agent pool spec does not exist (%s)", apc.agentPoolPath)
	}

	apc.agentPool, err = apiloader.LoadAgentPoolFromFile(apc.agentPoolPath)
	if err != nil {
		return errors.Wrap(err, "error parsing the agent pool")
	}

	if err = apc.authArgs.validateAuthArgs(); err != nil {
		return err
	}

	if apc.client, err = apc.authArgs.getClient(); err != nil {
		return errors.Wrap(err, "failed to get client")
	}

	_, err = apc.client.EnsureResourceGroup(ctx, apc.resourceGroupName, apc.location, nil)
	if err != nil {
		return err
	}

	if apc.containerService.Location == "" {
		apc.containerService.Location = apc.location
	} else if apc.containerService.Location != apc.location {
		return errors.New("--location does not match api model location")
	}

	//allows to identify VMs in the resource group that belong to this cluster.
	apc.nameSuffix = apc.containerService.Properties.GetClusterID()
	log.Debugf("Cluster ID used in all agent pools: %s", apc.nameSuffix)

	apc.kubeconfig, err = engine.GenerateKubeConfig(apc.containerService.Properties, apc.location)
	if err != nil {
		return errors.New("Unable to derive kubeconfig from api model")
	}
	return nil
}

func (apc *generateAddPoolCmd) run(cmd *cobra.Command, args []string) error {
	if err := apc.validate(cmd); err != nil {
		return errors.Wrap(err, "failed to validate scale command")
	}
	if err := apc.load(); err != nil {
		return errors.Wrap(err, "failed to load existing container service")
	}

	ctx, cancel := context.WithTimeout(context.Background(), armhelpers.DefaultARMOperationTimeout)
	defer cancel()
	orchestratorInfo := apc.containerService.Properties.OrchestratorProfile

	for vmssListPage, err := apc.client.ListVirtualMachineScaleSets(ctx, apc.resourceGroupName); vmssListPage.NotDone(); err = vmssListPage.NextWithContext(ctx) {
		if err != nil {
			return errors.Wrap(err, "failed to get VMSS list in the resource group")
		}
		for _, vmss := range vmssListPage.Values() {
			segments := strings.Split(*vmss.Name, "-")
			if len(segments) == 4 && segments[0] == "k8s" {
				vmssName := segments[1]
				if apc.agentPool.Name == vmssName {
					return errors.New("An agent pool with the given name already exists in the cluster")
				}
			}
		}
	}

	translator := engine.Context{
		Translator: &i18n.Translator{
			Locale: apc.locale,
		},
	}
	templateGenerator, err := engine.InitializeTemplateGenerator(translator)
	if err != nil {
		return errors.Wrap(err, "failed to initialize template generator")
	}

	apc.containerService.Properties.AgentPoolProfiles = []*api.AgentPoolProfile{apc.agentPool}

	certsGenerated, err := apc.containerService.SetPropertiesDefaults(api.PropertiesDefaultsParams{
		IsScale:    true,
		IsUpgrade:  false,
		PkiKeySize: helpers.DefaultPkiKeySize,
	})
	if err != nil {
		return errors.Wrapf(err, "error in SetPropertiesDefaults template %s", apc.apiModelPath)
	}
	template, parameters, err := templateGenerator.GenerateTemplateV2(apc.containerService, engine.DefaultGeneratorCode, BuildTag)
	if err != nil {
		return errors.Wrapf(err, "error generating template %s", apc.apiModelPath)
	}

	if !apc.noPrettyPrint {
		if template, err = transform.PrettyPrintArmTemplate(template); err != nil {
			return errors.Wrap(err, "error pretty-printing template")
		}
		if parameters, err = transform.BuildAzureParametersFile(parameters); err != nil {
			return errors.Wrap(err, "error pretty-printing template parameters")
		}
	}

	templateJSON := make(map[string]interface{})
	parametersJSON := make(map[string]interface{})

	err = json.Unmarshal([]byte(template), &templateJSON)
	if err != nil {
		return errors.Wrap(err, "error unmarshaling template")
	}

	err = json.Unmarshal([]byte(parameters), &parametersJSON)
	if err != nil {
		return errors.Wrap(err, "error unmarshaling parameters")
	}

	transformer := transform.Transformer{Translator: translator.Translator}

	//addValue(parametersJSON, apc.agentPool.Name+"Count", countForTemplate)
	addValue(parametersJSON, apc.agentPool.Name+"Count", 0)

	if orchestratorInfo.OrchestratorType == api.Kubernetes {
		if orchestratorInfo.KubernetesConfig.LoadBalancerSku == api.StandardLoadBalancerSku {
			err = transformer.NormalizeForK8sSLBScalingOrUpgrade(apc.logger, templateJSON)
			if err != nil {
				return errors.Wrapf(err, "error transforming the template for scaling with SLB %s", apc.apiModelPath)
			}
		}
		err = transformer.NormalizeForK8sVMASScalingUp(apc.logger, templateJSON)
		if err != nil {
			return errors.Wrapf(err, "error transforming the template for scaling template %s", apc.apiModelPath)
		}
	}
	/*
	random := rand.New(rand.NewSource(time.Now().UnixNano()))
	deploymentSuffix := random.Int31()

	_, err = apc.client.DeployTemplate(
		ctx,
		apc.resourceGroupName,
		fmt.Sprintf("%s-%d", apc.resourceGroupName, deploymentSuffix),
		templateJSON,
		parametersJSON)
	if err != nil {
		return err
	}
	if apc.nodes != nil {
		nodes, err := operations.GetNodes(apc.client, apc.logger, apc.apiserverURL, apc.kubeconfig, time.Duration(5)*time.Minute, apc.agentPool.Name, apc.agentPool.Count)
		if err == nil && nodes != nil {
			apc.nodes = nodes
			apc.logger.Infof("Nodes in pool '%s' after scaling:\n", apc.agentPool.Name)
			operations.PrintNodes(apc.nodes)
		} else {
			apc.logger.Warningf("Unable to get nodes in pool %s after scaling:\n", apc.agentPool.Name)
		}
	}
	*/
	
	newTemplate, _ := json.MarshalIndent(templateJSON, "", " ")
	newParameters, _ := json.MarshalIndent(parametersJSON, "", " ")

	writer := &engine.ArtifactWriter{
		Translator: &i18n.Translator{
			Locale: apc.locale,
		},
	}
	parametersOnly := false
	if err = writer.WriteTLSArtifacts(apc.containerService, apc.apiVersion, string(newTemplate), string(newParameters), apc.outputDirectory, certsGenerated, parametersOnly); err != nil {
		return errors.Wrap(err, "writing artifacts")
	}

	return apc.saveAPIModel()
}

func (apc *generateAddPoolCmd) saveAPIModel() error {
	var err error
	apiloader := &api.Apiloader{
		Translator: &i18n.Translator{
			Locale: apc.locale,
		},
	}
	var apiVersion string
	apc.containerService, apiVersion, err = apiloader.LoadContainerServiceFromFile(apc.apiModelPath, false, true, nil)
	if err != nil {
		return err
	}
	apc.containerService.Properties.AgentPoolProfiles[apc.agentPoolIndex].Count = apc.agentPool.Count

	b, err := apiloader.SerializeContainerService(apc.containerService, apiVersion)

	if err != nil {
		return err
	}

	f := helpers.FileSaver{
		Translator: &i18n.Translator{
			Locale: apc.locale,
		},
	}
	dir, file := filepath.Split(apc.apiModelPath)
	return f.SaveFile(dir, file, b)
}
