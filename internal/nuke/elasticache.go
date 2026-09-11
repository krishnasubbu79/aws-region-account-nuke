package nuke

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticache/types"
)

type ElastiCacheService struct {
	client *elasticache.Client
}

func NewElastiCacheService(config aws.Config) *ElastiCacheService {
	return &ElastiCacheService{client: elasticache.NewFromConfig(config)}
}

func (s *ElastiCacheService) Name() string { return "elasticache" }

func (s *ElastiCacheService) Discover(ctx context.Context) ([]Resource, error) {
	groups, err := s.replicationGroups(ctx)
	if err != nil {
		return nil, err
	}
	clusters, err := s.cacheClusters(ctx)
	if err != nil {
		return nil, err
	}
	serverless, err := s.serverlessCaches(ctx)
	if err != nil {
		return nil, err
	}
	return append(append(groups, clusters...), serverless...), nil
}

func (s *ElastiCacheService) replicationGroups(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	paginator := elasticache.NewDescribeReplicationGroupsPaginator(s.client, &elasticache.DescribeReplicationGroupsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, group := range page.ReplicationGroups {
			resource, err := s.resource(ctx, "elasticache-replication-group", aws.ToString(group.ReplicationGroupId), aws.ToString(group.ARN))
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func (s *ElastiCacheService) cacheClusters(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	paginator := elasticache.NewDescribeCacheClustersPaginator(s.client, &elasticache.DescribeCacheClustersInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cluster := range page.CacheClusters {
			// A replication group's member clusters are removed with the group.
			if aws.ToString(cluster.ReplicationGroupId) != "" {
				continue
			}
			resource, err := s.resource(ctx, "elasticache-cluster", aws.ToString(cluster.CacheClusterId), aws.ToString(cluster.ARN))
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func (s *ElastiCacheService) serverlessCaches(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	paginator := elasticache.NewDescribeServerlessCachesPaginator(s.client, &elasticache.DescribeServerlessCachesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cache := range page.ServerlessCaches {
			resource, err := s.resource(ctx, "elasticache-serverless-cache", aws.ToString(cache.ServerlessCacheName), aws.ToString(cache.ARN))
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func (s *ElastiCacheService) resource(ctx context.Context, resourceType, name, arn string) (Resource, error) {
	tagOutput, err := s.client.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{ResourceName: &arn})
	if err != nil {
		return Resource{}, fmt.Errorf("list tags for %s: %w", name, err)
	}
	return Resource{Service: s.Name(), Type: resourceType, ID: name, Name: name, ARN: arn, Tags: cacheTags(tagOutput.TagList)}, nil
}

func cacheTags(tags []types.Tag) map[string]string {
	result := make(map[string]string, len(tags))
	for _, tag := range tags {
		result[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return result
}

func (s *ElastiCacheService) Delete(ctx context.Context, resource Resource) error {
	tagOutput, err := s.client.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{ResourceName: &resource.ARN})
	if err != nil {
		return err
	}
	if err := refuseIfProtected(resource.Name, cacheTags(tagOutput.TagList)); err != nil {
		return err
	}
	switch resource.Type {
	case "elasticache-replication-group":
		_, err := s.client.DeleteReplicationGroup(ctx, &elasticache.DeleteReplicationGroupInput{ReplicationGroupId: &resource.ID})
		return err
	case "elasticache-cluster":
		_, err := s.client.DeleteCacheCluster(ctx, &elasticache.DeleteCacheClusterInput{CacheClusterId: &resource.ID})
		return err
	case "elasticache-serverless-cache":
		_, err := s.client.DeleteServerlessCache(ctx, &elasticache.DeleteServerlessCacheInput{ServerlessCacheName: &resource.ID})
		return err
	default:
		return fmt.Errorf("unknown ElastiCache resource type %q", resource.Type)
	}
}

func (s *ElastiCacheService) Wait(ctx context.Context, resource Resource) error {
	return waitUntilGone(ctx, func(ctx context.Context) error {
		switch resource.Type {
		case "elasticache-replication-group":
			_, err := s.client.DescribeReplicationGroups(ctx, &elasticache.DescribeReplicationGroupsInput{ReplicationGroupId: &resource.ID})
			return err
		case "elasticache-cluster":
			_, err := s.client.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{CacheClusterId: &resource.ID})
			return err
		case "elasticache-serverless-cache":
			_, err := s.client.DescribeServerlessCaches(ctx, &elasticache.DescribeServerlessCachesInput{ServerlessCacheName: &resource.ID})
			return err
		default:
			return fmt.Errorf("unknown ElastiCache resource type %q", resource.Type)
		}
	})
}
