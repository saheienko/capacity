package aws

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"
	"github.com/saheienko/supergiant/pkg/clouds/aws"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/supergiant/capacity/pkg/provider"
)

// Provider name:
const (
	Name = "aws"
)

// AWS instance parameters:
const (
	KeyID          = "awsKeyID"
	SecretKey      = "awsSecretKey"
	Region         = "awsRegion"
	KeyName        = "awsKeyName"
	ImageID        = "awsImageID"
	IAMRole        = "awsIAMRole"
	SecurityGroups = "awsSecurityGroups"
	SubnetID       = "awsSubnetID"
	VolType        = "awsVolType"
	VolSize        = "awsVolSize"
	EBSOptimized   = "ebsOptimized"
	Tags           = "awsTags"
)

type Config struct {
	KeyName        string
	ImageID        string
	IAMRole        string
	SecurityGroups []*string
	SubnetID       string
	VolType        string
	VolSize        int64
	EBSOptimized   *bool
	Tags           map[string]string
}

type Provider struct {
	clusterName string
	region      string
	instConf    Config
	client      *aws.Client
}

func New(clusterName string, config provider.Config) (*Provider, error) {
	// TODO: parse and validate config
	key, secret, region := config[KeyID], config[SecretKey], config[Region]

	// TODO: review tags behavior, it would be better to change this filter dynamically
	tags := provider.ParseMap(config[Tags])
	if tags == nil {
		tags = make(map[string]string)
	}
	tags[provider.TagCluster] = clusterName

	client, err := aws.New(key, secret, tags)
	if err != nil {
		return nil, err
	}

	return &Provider{
		clusterName: clusterName,
		region:      region,
		instConf: Config{
			KeyName:        config[KeyName],
			ImageID:        config[ImageID],
			IAMRole:        config[IAMRole],
			SecurityGroups: provider.ParseList(config[SecurityGroups]),
			SubnetID:       config[SubnetID],
			VolType:        config[VolType],
			VolSize:        int64(100),
			EBSOptimized:   parseBool(config[EBSOptimized]),
			Tags:           tags,
		},
		client: client,
	}, nil
}

func (p *Provider) Name() string {
	return "aws"
}

func (p *Provider) MachineTypes(ctx context.Context) ([]*provider.MachineType, error) {
	// TODO: for each region aws supports different machine types (get just region ones)
	instTypes, err := p.client.AvailableInstanceTypes(ctx)
	if err != nil {
		return nil, err
	}

	mTypes := make([]*provider.MachineType, 0, len(instTypes))
	for i := range instTypes {
		mem, err := parseMemory(instTypes[i].Attributes.Memory)
		if err != nil {
			return nil, errors.Wrapf(err, "memory: parse %s", instTypes[i].Attributes.Memory)
		}
		cpu, err := parseVCPU(instTypes[i].Attributes.VCPU)
		if err != nil {
			return nil, errors.Wrapf(err, "vcpu: parse %s", instTypes[i].Attributes.VCPU)
		}
		mTypes = append(mTypes, &provider.MachineType{
			Name:           instTypes[i].Attributes.InstanceType,
			Memory:         instTypes[i].Attributes.Memory,
			CPU:            instTypes[i].Attributes.VCPU,
			MemoryResource: mem,
			CPUResource:    cpu,
		})
	}

	return mTypes, nil
}

func (p *Provider) GetMachine(ctx context.Context, id string) (*provider.Machine, error) {
	inst, err := p.client.GetInstance(ctx, p.region, id)
	if err != nil {
		return nil, err
	}
	return machineFrom(inst), nil
}

func (p *Provider) Machines(ctx context.Context) ([]*provider.Machine, error) {
	insts, err := p.client.ListRegionInstances(ctx, p.region, nil)
	if err != nil {
		return nil, nil
	}

	machines := make([]*provider.Machine, len(insts))
	for i := range insts {
		machines[i] = machineFrom(insts[i])
	}

	return machines, nil
}

func (p *Provider) CreateMachine(ctx context.Context, name, mtype, clusterRole, userData string, config provider.Config) (*provider.Machine, error) {
	// TODO: merge and validate config parameters

	inst, err := p.client.CreateInstance(ctx, aws.InstanceConfig{
		TagName:        name,
		TagClusterName: p.clusterName,
		TagClusterRole: clusterRole,
		Type:           mtype,
		Region:         p.region,
		ImageID:        p.instConf.ImageID,
		KeyName:        p.instConf.KeyName,
		IAMRole:        p.instConf.IAMRole,
		SecurityGroups: p.instConf.SecurityGroups,
		SubnetID:       p.instConf.SubnetID,
		VolumeType:     p.instConf.VolType,
		VolumeSize:     p.instConf.VolSize,
		EBSOptimized:   p.instConf.EBSOptimized,
		Tags:           p.instConf.Tags,
		UsedData:       base64.StdEncoding.EncodeToString([]byte(userData)),
	})
	if err != nil {
		return nil, err
	}

	return machineFrom(inst), nil
}

func (p *Provider) DeleteMachine(ctx context.Context, id string) (*provider.Machine, error) {
	instState, err := p.client.DeleteInstance(ctx, p.region, id)
	if err != nil {
		return nil, err
	}
	return &provider.Machine{
		ID:    id,
		State: toString(instState.CurrentState),
	}, nil
}

func normalizeMemory(memory string) string {
	// "1 GiB" --> "1Gi"
	fixed := strings.Trim(strings.Replace(memory, " ", "", -1), "B")

	// Some inst types uses comma for float types - x1.32xlarge: 1,952 GiB
	fixed = strings.Replace(fixed, ",", ".", -1)

	return fixed
}

func parseMemory(memory string) (resource.Quantity, error) {
	return resource.ParseQuantity(normalizeMemory(memory))
}

func parseVCPU(vcpu string) (resource.Quantity, error) {
	return resource.ParseQuantity(vcpu)
}

func getName(tags []*ec2.Tag) string {
	for _, tag := range tags {
		if *tag.Key == "Name" {
			return *tag.Value
		}
	}
	return ""
}

func toString(state *ec2.InstanceState) string {
	if state == nil {
		return ""
	}
	return *state.Name
}

func machineFrom(inst *ec2.Instance) *provider.Machine {
	return &provider.Machine{
		ID:                *inst.InstanceId,
		Name:              getName(inst.Tags),
		Type:              *inst.InstanceType,
		CreationTimestamp: *inst.LaunchTime,
		State:             toString(inst.State),
	}
}

func parseBool(s string) *bool {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return nil
	}
	return &b
}
