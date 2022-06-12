package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/helm/v3"
	. "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
	"github.com/rancher/opni/infra/pkg/aws"
	"github.com/rancher/opni/infra/pkg/resources"
	"golang.org/x/mod/semver"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
)

func main() {
	Run(run)
}

type ClusterConfig struct {
	NodeInstanceType     string `json:"nodeInstanceType"`
	NodeGroupMinSize     int    `json:"nodeGroupMinSize"`
	NodeGroupMaxSize     int    `json:"nodeGroupMaxSize"`
	NodeGroupDesiredSize int    `json:"nodeGroupDesiredSize"`
}

func run(ctx *Context) (runErr error) {
	defer func() {
		if sr, ok := runErr.(interface {
			StackTrace() errors.StackTrace
		}); ok {
			st := sr.StackTrace()
			ctx.Log.Error(fmt.Sprintf("%+v", st), &LogArgs{})
		}
	}()

	var infraConfig ClusterConfig
	namePrefix := config.Require(ctx, "namePrefix")
	zoneID := config.Require(ctx, "zoneID")
	useLocalCharts := config.GetBool(ctx, "useLocalCharts")
	config.RequireObject(ctx, "cluster", &infraConfig)

	var cloud, imageRepo, imageTag string

	if value, ok := os.LookupEnv("CLOUD"); ok {
		cloud = value
	} else {
		cloud = config.Require(ctx, "cloud")
	}
	if value, ok := os.LookupEnv("IMAGE_REPO"); ok {
		imageRepo = value
	} else {
		imageRepo = config.Require(ctx, "imageRepo")
	}
	if value, ok := os.LookupEnv("IMAGE_TAG"); ok {
		imageTag = value
	} else {
		imageTag = config.Require(ctx, "imageTag")
	}

	var provisioner resources.Provisioner

	switch cloud {
	case "aws":
		provisioner = aws.NewProvisioner()
	default:
		return errors.Errorf("unsupported cloud: %s", cloud)
	}

	var id StringOutput
	if rand, err := random.NewRandomId(ctx, "id", &random.RandomIdArgs{
		ByteLength: Int(4),
	}); err != nil {
		return errors.WithStack(err)
	} else {
		id = rand.Hex
	}

	conf := resources.MainClusterConfig{
		ID:                   id,
		NamePrefix:           namePrefix,
		NodeInstanceType:     infraConfig.NodeInstanceType,
		NodeGroupMinSize:     infraConfig.NodeGroupMinSize,
		NodeGroupMaxSize:     infraConfig.NodeGroupMaxSize,
		NodeGroupDesiredSize: infraConfig.NodeGroupDesiredSize,
		ZoneID:               zoneID,
	}

	mainCluster, err := provisioner.ProvisionMainCluster(ctx, conf)
	if err != nil {
		return err
	}

	var opniCrdChart, opniPrometheusCrdChart, opniChart, opniAgentChart string
	var chartRepoOpts *helm.RepositoryOptsArgs
	if useLocalCharts {
		chartRepoOpts = &helm.RepositoryOptsArgs{
			Repo: StringPtr("https://raw.githubusercontent.com/rancher/opni/charts-repo/"),
		}
		var ok bool
		if opniCrdChart, ok = findLocalChartDir("opni-crd"); !ok {
			return errors.New("could not find local opni-crd chart")
		}
		if opniPrometheusCrdChart, ok = findLocalChartDir("opni-prometheus-crd"); !ok {
			return errors.New("could not find local opni-prometheus-crd chart")
		}
		if opniChart, ok = findLocalChartDir("opni"); !ok {
			return errors.New("could not find local opni chart")
		}
		if opniAgentChart, ok = findLocalChartDir("opni-agent"); !ok {
			return errors.New("could not find local opni-agent chart")
		}
	} else {
		opniCrdChart = "opni-crd"
		opniPrometheusCrdChart = "opni-prometheus-crd"
		opniChart = "opni"
		opniAgentChart = "opni-agent"
	}

	opniServiceLB := mainCluster.Provider.ApplyT(func(k *kubernetes.Provider) (StringOutput, error) {
		opniPrometheusCrd, err := helm.NewRelease(ctx, "opni-prometheus-crd", &helm.ReleaseArgs{
			Chart:          String(opniPrometheusCrdChart),
			RepositoryOpts: chartRepoOpts,
			Namespace:      String("opni"),
			Atomic:         Bool(true),
			ForceUpdate:    Bool(true),
			Timeout:        Int(60),
		}, Provider(k))
		if err != nil {
			return StringOutput{}, errors.WithStack(err)
		}

		opniCrd, err := helm.NewRelease(ctx, "opni-crd", &helm.ReleaseArgs{
			Chart:          String(opniCrdChart),
			RepositoryOpts: chartRepoOpts,
			Namespace:      String("opni"),
			Atomic:         Bool(true),
			ForceUpdate:    Bool(true),
			Timeout:        Int(60),
		}, Provider(k), DependsOn([]Resource{opniPrometheusCrd}))
		if err != nil {
			return StringOutput{}, errors.WithStack(err)
		}

		opni, err := helm.NewRelease(ctx, "opni", &helm.ReleaseArgs{
			Chart:          String(opniChart),
			RepositoryOpts: chartRepoOpts,
			Namespace:      String("opni"),
			Values: Map{
				"image": Map{
					"repository": String(imageRepo),
					"tag":        String(imageTag),
				},
				"gateway": Map{
					"enabled":     Bool(true),
					"hostname":    mainCluster.GatewayHostname,
					"serviceType": String("LoadBalancer"),
					"auth": Map{
						"provider": String("openid"),
						"openid": Map{
							"discovery": Map{
								"issuer": mainCluster.OAuth.Issuer,
							},
							"identifyingClaim":  String("email"),
							"clientID":          mainCluster.OAuth.ClientID,
							"clientSecret":      mainCluster.OAuth.ClientSecret,
							"scopes":            ToStringArray([]string{"openid", "profile", "email"}),
							"roleAttributePath": String(`'"custom:grafana_role"'`),
						},
					},
				},
				"monitoring": Map{
					"enabled": Bool(true),
					"grafana": Map{
						"enabled":  Bool(true),
						"hostname": mainCluster.GrafanaHostname,
					},
					"cortex": Map{
						"storage": Map{
							"backend": String("s3"),
							"s3": Map{
								"endpoint":         mainCluster.S3.Endpoint,
								"region":           mainCluster.S3.Region,
								"bucketName":       mainCluster.S3.Bucket,
								"accessKeyID":      mainCluster.S3.AccessKeyID,
								"secretAccessKey":  mainCluster.S3.SecretAccessKey,
								"signatureVersion": String("v4"),
							},
						},
					},
				},
			},
			Atomic:      Bool(true),
			ForceUpdate: Bool(true),
			Timeout:     Int(300),
		}, Provider(k), DependsOn([]Resource{opniPrometheusCrd, opniCrd}))
		if err != nil {
			return StringOutput{}, errors.WithStack(err)
		}

		_, err = helm.NewRelease(ctx, "opni-agent", &helm.ReleaseArgs{
			Chart:           String(opniAgentChart),
			RepositoryOpts:  chartRepoOpts,
			Namespace:       String("opni"),
			CreateNamespace: Bool(true),
			Values: Map{
				"address": String("opni-monitoring.opni.svc:9090"),
				"image": Map{
					"repository": String(imageRepo),
					"tag":        String(imageTag),
				},
				"metrics": Map{
					"enabled": Bool(true),
				},
				"bootstrapInCluster": Map{
					"enabled":           Bool(true),
					"managementAddress": String("opni-monitoring-internal.opni.svc:11090"),
				},
				"kube-prometheus-stack": Map{
					"enabled": Bool(true),
				},
			},
			Atomic:      Bool(true),
			ForceUpdate: Bool(true),
			Timeout:     Int(300),
		}, Provider(k), DependsOn([]Resource{opniPrometheusCrd, opniCrd, opni}))
		if err != nil {
			return StringOutput{}, errors.WithStack(err)
		}

		opniServiceLB := All(opni.Status.Namespace(), opni.Status.Name()).
			ApplyT(func(args []any) (StringOutput, error) {
				timeout, ca := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ca()
				namespace := args[0].(*string)
				var opniLBSvc *corev1.Service
				for timeout.Err() == nil {
					opniLBSvc, err = corev1.GetService(ctx, "opni-monitoring", ID(
						fmt.Sprintf("%s/opni-monitoring", *namespace),
					), nil, Provider(k), Parent(opni), Timeouts(&CustomTimeouts{
						Create: (5 * time.Minute).String(),
						Update: (5 * time.Minute).String(),
					}))
					if err != nil {
						time.Sleep(time.Second * 1)
						continue
					}
					break
				}
				if timeout.Err() != nil {
					return StringOutput{}, errors.WithStack(timeout.Err())
				}
				return opniLBSvc.Status.LoadBalancer().
					Ingress().Index(Int(0)).Hostname().Elem(), nil
			}).(StringOutput)
		return opniServiceLB, nil
	}).(StringOutput)

	_, err = provisioner.ProvisionDNSRecord(ctx, "gateway", resources.DNSRecordConfig{
		Name:    mainCluster.GatewayHostname,
		Type:    "CNAME",
		ZoneID:  conf.ZoneID,
		Records: StringArray{opniServiceLB},
		TTL:     60,
	})
	if err != nil {
		return errors.WithStack(err)
	}

	_, err = provisioner.ProvisionDNSRecord(ctx, "grafana", resources.DNSRecordConfig{
		Name:    mainCluster.GrafanaHostname,
		Type:    "CNAME",
		ZoneID:  conf.ZoneID,
		Records: StringArray{mainCluster.LoadBalancerHostname},
		TTL:     60,
	})
	if err != nil {
		return errors.WithStack(err)
	}

	ctx.Export("kubeconfig", mainCluster.Kubeconfig)
	ctx.Export("gateway_url", mainCluster.GatewayHostname)
	ctx.Export("grafana_url", mainCluster.GrafanaHostname.ApplyT(func(hostname string) string {
		return fmt.Sprintf("https://%s", hostname)
	}).(StringOutput))
	ctx.Export("s3_bucket", mainCluster.S3.Bucket)
	ctx.Export("s3_endpoint", mainCluster.S3.Endpoint)
	ctx.Export("oauth_client_id", mainCluster.OAuth.ClientID)
	ctx.Export("oauth_client_secret", mainCluster.OAuth.ClientSecret)
	ctx.Export("oauth_issuer_url", mainCluster.OAuth.Issuer)
	return nil
}

func findLocalChartDir(chartName string) (string, bool) {
	// find charts from ../charts/<chartName> and return the latest version
	dir := fmt.Sprintf("../charts/%s", chartName)
	if _, err := os.Stat(dir); err != nil {
		return "", false
	}
	versions, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	if len(versions) == 0 {
		return "", false
	}
	names := make([]string, 0, len(versions))
	for _, version := range versions {
		if version.IsDir() {
			names = append(names, version.Name())
		}
	}
	semver.Sort(names)
	return filepath.Join(dir, names[len(names)-1]), true
}
