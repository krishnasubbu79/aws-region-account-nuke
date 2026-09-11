package nuke

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kafka"
)

type MSKService struct {
	client *kafka.Client
}

func NewMSKService(config aws.Config) *MSKService {
	return &MSKService{client: kafka.NewFromConfig(config)}
}

func (s *MSKService) Name() string { return "msk" }

func (s *MSKService) Discover(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	paginator := kafka.NewListClustersV2Paginator(s.client, &kafka.ListClustersV2Input{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, cluster := range page.ClusterInfoList {
			arn := aws.ToString(cluster.ClusterArn)
			tags, err := s.client.ListTagsForResource(ctx, &kafka.ListTagsForResourceInput{ResourceArn: &arn})
			if err != nil {
				return nil, err
			}
			resources = append(resources, Resource{
				Service: s.Name(), Type: "msk-cluster", ID: arn,
				Name: aws.ToString(cluster.ClusterName), ARN: arn, Tags: tags.Tags,
			})
		}
	}
	return resources, nil
}

func (s *MSKService) Delete(ctx context.Context, resource Resource) error {
	tags, err := s.client.ListTagsForResource(ctx, &kafka.ListTagsForResourceInput{ResourceArn: &resource.ARN})
	if err != nil {
		return err
	}
	if err := refuseIfProtected(resource.Name, tags.Tags); err != nil {
		return err
	}
	_, err = s.client.DeleteCluster(ctx, &kafka.DeleteClusterInput{ClusterArn: &resource.ARN})
	return err
}

func (s *MSKService) Wait(ctx context.Context, resource Resource) error {
	return waitUntilGone(ctx, func(ctx context.Context) error {
		_, err := s.client.DescribeClusterV2(ctx, &kafka.DescribeClusterV2Input{ClusterArn: &resource.ARN})
		return err
	})
}
