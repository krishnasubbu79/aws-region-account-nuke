package nuke

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/opensearch"
	opensearchtypes "github.com/aws/aws-sdk-go-v2/service/opensearch/types"
	"github.com/aws/aws-sdk-go-v2/service/opensearchserverless"
	serverlesstypes "github.com/aws/aws-sdk-go-v2/service/opensearchserverless/types"
)

type OpenSearchService struct {
	managed    *opensearch.Client
	serverless *opensearchserverless.Client
}

func NewOpenSearchService(config aws.Config) *OpenSearchService {
	return &OpenSearchService{
		managed:    opensearch.NewFromConfig(config),
		serverless: opensearchserverless.NewFromConfig(config),
	}
}

func (s *OpenSearchService) Name() string { return "opensearch" }

func (s *OpenSearchService) Discover(ctx context.Context) ([]Resource, error) {
	managed, err := s.managedDomains(ctx)
	if err != nil {
		return nil, err
	}
	serverless, err := s.serverlessCollections(ctx)
	if err != nil {
		return nil, err
	}
	return append(managed, serverless...), nil
}

func (s *OpenSearchService) managedDomains(ctx context.Context) ([]Resource, error) {
	output, err := s.managed.ListDomainNames(ctx, &opensearch.ListDomainNamesInput{})
	if err != nil {
		return nil, err
	}
	resources := make([]Resource, 0, len(output.DomainNames))
	for _, summary := range output.DomainNames {
		name := aws.ToString(summary.DomainName)
		details, err := s.managed.DescribeDomain(ctx, &opensearch.DescribeDomainInput{DomainName: &name})
		if err != nil {
			return nil, err
		}
		arn := aws.ToString(details.DomainStatus.ARN)
		tags, err := s.managed.ListTags(ctx, &opensearch.ListTagsInput{ARN: &arn})
		if err != nil {
			return nil, err
		}
		resources = append(resources, Resource{
			Service: s.Name(), Type: "opensearch-domain", ID: name, Name: name, ARN: arn,
			Tags: managedSearchTags(tags.TagList),
		})
	}
	return resources, nil
}

func managedSearchTags(tags []opensearchtypes.Tag) map[string]string {
	result := make(map[string]string, len(tags))
	for _, tag := range tags {
		result[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return result
}

func (s *OpenSearchService) serverlessCollections(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	paginator := opensearchserverless.NewListCollectionsPaginator(s.serverless, &opensearchserverless.ListCollectionsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, collection := range page.CollectionSummaries {
			arn := aws.ToString(collection.Arn)
			tags, err := s.serverless.ListTagsForResource(ctx, &opensearchserverless.ListTagsForResourceInput{ResourceArn: &arn})
			if err != nil {
				return nil, err
			}
			resources = append(resources, Resource{
				Service: s.Name(), Type: "opensearch-serverless-collection",
				ID: aws.ToString(collection.Id), Name: aws.ToString(collection.Name), ARN: arn,
				Tags: serverlessSearchTags(tags.Tags),
			})
		}
	}
	return resources, nil
}

func serverlessSearchTags(tags []serverlesstypes.Tag) map[string]string {
	result := make(map[string]string, len(tags))
	for _, tag := range tags {
		result[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return result
}

func (s *OpenSearchService) Delete(ctx context.Context, resource Resource) error {
	switch resource.Type {
	case "opensearch-domain":
		tags, err := s.managed.ListTags(ctx, &opensearch.ListTagsInput{ARN: &resource.ARN})
		if err != nil {
			return err
		}
		if err := refuseIfProtected(resource.Name, managedSearchTags(tags.TagList)); err != nil {
			return err
		}
		_, err = s.managed.DeleteDomain(ctx, &opensearch.DeleteDomainInput{DomainName: &resource.ID})
		return err
	case "opensearch-serverless-collection":
		tags, err := s.serverless.ListTagsForResource(ctx, &opensearchserverless.ListTagsForResourceInput{ResourceArn: &resource.ARN})
		if err != nil {
			return err
		}
		if err := refuseIfProtected(resource.Name, serverlessSearchTags(tags.Tags)); err != nil {
			return err
		}
		_, err = s.serverless.DeleteCollection(ctx, &opensearchserverless.DeleteCollectionInput{Id: &resource.ID})
		return err
	default:
		return fmt.Errorf("unknown OpenSearch resource type %q", resource.Type)
	}
}

func (s *OpenSearchService) Wait(ctx context.Context, resource Resource) error {
	return waitUntilGone(ctx, func(ctx context.Context) error {
		switch resource.Type {
		case "opensearch-domain":
			_, err := s.managed.DescribeDomain(ctx, &opensearch.DescribeDomainInput{DomainName: &resource.ID})
			return err
		case "opensearch-serverless-collection":
			output, err := s.serverless.BatchGetCollection(ctx, &opensearchserverless.BatchGetCollectionInput{Ids: []string{resource.ID}})
			if err == nil && len(output.CollectionDetails) == 0 {
				return errResourceGone
			}
			return err
		default:
			return fmt.Errorf("unknown OpenSearch resource type %q", resource.Type)
		}
	})
}
