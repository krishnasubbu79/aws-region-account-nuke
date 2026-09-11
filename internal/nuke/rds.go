package nuke

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

type RDSService struct {
	client                    *rds.Client
	disableDeletionProtection bool
}

func NewRDSService(config aws.Config, disableDeletionProtection bool) *RDSService {
	return &RDSService{
		client:                    rds.NewFromConfig(config),
		disableDeletionProtection: disableDeletionProtection,
	}
}

func (s *RDSService) Name() string { return "rds" }

func (s *RDSService) Discover(ctx context.Context) ([]Resource, error) {
	clusters, err := s.auroraClusters(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := s.standaloneInstances(ctx)
	if err != nil {
		return nil, err
	}
	return append(clusters, instances...), nil
}

func (s *RDSService) auroraClusters(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	paginator := rds.NewDescribeDBClustersPaginator(s.client, &rds.DescribeDBClustersInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cluster := range page.DBClusters {
			if !strings.HasPrefix(aws.ToString(cluster.Engine), "aurora") {
				continue
			}
			resource, err := s.resource(ctx, "aurora-cluster", aws.ToString(cluster.DBClusterIdentifier), aws.ToString(cluster.DBClusterArn))
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func (s *RDSService) standaloneInstances(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	paginator := rds.NewDescribeDBInstancesPaginator(s.client, &rds.DescribeDBInstancesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, instance := range page.DBInstances {
			if aws.ToString(instance.DBClusterIdentifier) != "" {
				continue
			}
			resource, err := s.resource(ctx, "rds-instance", aws.ToString(instance.DBInstanceIdentifier), aws.ToString(instance.DBInstanceArn))
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func (s *RDSService) resource(ctx context.Context, resourceType, name, arn string) (Resource, error) {
	output, err := s.client.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: &arn})
	if err != nil {
		return Resource{}, fmt.Errorf("list tags for %s: %w", name, err)
	}
	return Resource{Service: s.Name(), Type: resourceType, ID: name, Name: name, ARN: arn, Tags: rdsTags(output.TagList)}, nil
}

func rdsTags(tags []types.Tag) map[string]string {
	result := make(map[string]string, len(tags))
	for _, tag := range tags {
		result[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return result
}

func (s *RDSService) Delete(ctx context.Context, resource Resource) error {
	tags, err := s.client.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: &resource.ARN})
	if err != nil {
		return err
	}
	if err := refuseIfProtected(resource.Name, rdsTags(tags.TagList)); err != nil {
		return err
	}
	switch resource.Type {
	case "aurora-cluster":
		return s.deleteAuroraCluster(ctx, resource.ID)
	case "rds-instance":
		return s.deleteInstance(ctx, resource.ID)
	default:
		return fmt.Errorf("unknown RDS resource type %q", resource.Type)
	}
}

func (s *RDSService) deleteAuroraCluster(ctx context.Context, id string) error {
	output, err := s.client.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{DBClusterIdentifier: &id})
	if err != nil {
		return err
	}
	cluster := output.DBClusters[0]
	// Validate every member before changing or deleting anything, so a protected
	// member cannot leave an Aurora cluster half-deleted.
	for _, member := range cluster.DBClusterMembers {
		memberID := aws.ToString(member.DBInstanceIdentifier)
		instanceOutput, err := s.client.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: &memberID})
		if err != nil {
			return err
		}
		instance := instanceOutput.DBInstances[0]
		tagOutput, err := s.client.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: instance.DBInstanceArn})
		if err != nil {
			return err
		}
		if IsProtected(Resource{Tags: rdsTags(tagOutput.TagList)}) {
			return fmt.Errorf("Aurora member %s is tagged DoNotDelete=true", memberID)
		}
		if aws.ToBool(instance.DeletionProtection) && !s.disableDeletionProtection {
			return fmt.Errorf("Aurora member %s has deletion protection enabled", memberID)
		}
	}
	if aws.ToBool(cluster.DeletionProtection) {
		if !s.disableDeletionProtection {
			return fmt.Errorf("Aurora cluster %s has deletion protection enabled", id)
		}
		_, err = s.client.ModifyDBCluster(ctx, &rds.ModifyDBClusterInput{
			DBClusterIdentifier: &id, DeletionProtection: aws.Bool(false), ApplyImmediately: aws.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("disable deletion protection for %s: %w", id, err)
		}
		waiter := rds.NewDBClusterAvailableWaiter(s.client)
		if err := waiter.Wait(ctx, &rds.DescribeDBClustersInput{DBClusterIdentifier: &id}, 30*time.Minute); err != nil {
			return fmt.Errorf("wait after disabling deletion protection for %s: %w", id, err)
		}
	}

	for _, member := range cluster.DBClusterMembers {
		memberID := aws.ToString(member.DBInstanceIdentifier)
		if err := s.deleteInstance(ctx, memberID); err != nil {
			return fmt.Errorf("delete Aurora member %s: %w", memberID, err)
		}
	}
	for _, member := range cluster.DBClusterMembers {
		memberID := aws.ToString(member.DBInstanceIdentifier)
		if err := waitForDBInstanceGone(ctx, s.client, memberID); err != nil {
			return fmt.Errorf("wait for Aurora member %s: %w", memberID, err)
		}
	}

	_, err = s.client.DeleteDBCluster(ctx, &rds.DeleteDBClusterInput{
		DBClusterIdentifier: &id, SkipFinalSnapshot: aws.Bool(true), DeleteAutomatedBackups: aws.Bool(false),
	})
	return err
}

func (s *RDSService) deleteInstance(ctx context.Context, id string) error {
	output, err := s.client.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: &id})
	if err != nil {
		return err
	}
	instance := output.DBInstances[0]
	tags, err := s.client.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: instance.DBInstanceArn})
	if err != nil {
		return err
	}
	if err := refuseIfProtected(id, rdsTags(tags.TagList)); err != nil {
		return err
	}
	if aws.ToBool(instance.DeletionProtection) {
		if !s.disableDeletionProtection {
			return fmt.Errorf("RDS instance %s has deletion protection enabled", id)
		}
		_, err = s.client.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: &id, DeletionProtection: aws.Bool(false), ApplyImmediately: aws.Bool(true),
		})
		if err != nil {
			return fmt.Errorf("disable deletion protection for %s: %w", id, err)
		}
		waiter := rds.NewDBInstanceAvailableWaiter(s.client)
		if err := waiter.Wait(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: &id}, 30*time.Minute); err != nil {
			return fmt.Errorf("wait after disabling deletion protection for %s: %w", id, err)
		}
	}
	_, err = s.client.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: &id, SkipFinalSnapshot: aws.Bool(true), DeleteAutomatedBackups: aws.Bool(false),
	})
	return err
}

func (s *RDSService) Wait(ctx context.Context, resource Resource) error {
	switch resource.Type {
	case "aurora-cluster":
		return waitUntilGone(ctx, func(ctx context.Context) error {
			_, err := s.client.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{DBClusterIdentifier: &resource.ID})
			return err
		})
	case "rds-instance":
		return waitForDBInstanceGone(ctx, s.client, resource.ID)
	default:
		return fmt.Errorf("unknown RDS resource type %q", resource.Type)
	}
}

func waitForDBInstanceGone(ctx context.Context, client *rds.Client, id string) error {
	return waitUntilGone(ctx, func(ctx context.Context) error {
		_, err := client.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: &id})
		return err
	})
}
