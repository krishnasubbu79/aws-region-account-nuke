package nuke

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/efs/types"
)

type EFSService struct {
	client   *efs.Client
	selector Selector
	region   string
	// Serialize EFS deletion so two selected replication peers cannot race
	// to remove the same configuration.
	mu sync.Mutex
}

func NewEFSService(config aws.Config, selector Selector) *EFSService {
	return &EFSService{client: efs.NewFromConfig(config), selector: selector, region: config.Region}
}

func (s *EFSService) Name() string { return "efs" }

func efsResource(fs types.FileSystemDescription) Resource {
	tags := make(map[string]string, len(fs.Tags))
	for _, tag := range fs.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	name := aws.ToString(fs.Name)
	if name == "" {
		name = aws.ToString(fs.FileSystemId)
	}
	return Resource{Service: "efs", Type: "efs-file-system", ID: aws.ToString(fs.FileSystemId),
		Name: name, ARN: aws.ToString(fs.FileSystemArn), Tags: tags}
}

func (s *EFSService) Discover(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	pages := efs.NewDescribeFileSystemsPaginator(s.client, &efs.DescribeFileSystemsInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, fs := range page.FileSystems {
			resource := efsResource(fs)
			if s.selector.Matches(resource) && !IsProtected(resource) {
				if _, err := s.replication(ctx, resource.ID); err != nil {
					return nil, err
				}
			}
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func (s *EFSService) fileSystem(ctx context.Context, id string) (types.FileSystemDescription, error) {
	output, err := s.client.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{FileSystemId: &id})
	if err != nil {
		return types.FileSystemDescription{}, err
	}
	if len(output.FileSystems) != 1 {
		return types.FileSystemDescription{}, fmt.Errorf("expected one EFS file system for %s", id)
	}
	return output.FileSystems[0], nil
}

// Validate both ends before detaching replication, including when targeting
// the destination. Replication deletion affects both ends, not just the target.
func (s *EFSService) replication(ctx context.Context, id string) ([]types.ReplicationConfigurationDescription, error) {
	var replications []types.ReplicationConfigurationDescription
	pages := efs.NewDescribeReplicationConfigurationsPaginator(s.client, &efs.DescribeReplicationConfigurationsInput{FileSystemId: &id})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		var missing *types.ReplicationNotFound
		if errors.As(err, &missing) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		replications = append(replications, page.Replications...)
	}
	for _, replication := range replications {
		if aws.ToString(replication.SourceFileSystemRegion) != s.region {
			return nil, fmt.Errorf("EFS %s has cross-region replication; detach it explicitly outside this CLI", id)
		}
		source, err := s.fileSystem(ctx, aws.ToString(replication.SourceFileSystemId))
		if err != nil {
			return nil, fmt.Errorf("inspect EFS replication source: %w", err)
		}
		if aws.ToString(source.FileSystemArn) != aws.ToString(replication.SourceFileSystemArn) {
			return nil, fmt.Errorf("EFS %s replication source is outside this account", id)
		}
		peers := []types.FileSystemDescription{source}
		for _, destination := range replication.Destinations {
			if aws.ToString(destination.Region) != s.region ||
				(destination.OwnerId != nil && aws.ToString(destination.OwnerId) != aws.ToString(source.OwnerId)) {
				return nil, fmt.Errorf("EFS %s has cross-region or cross-account replication; detach it explicitly outside this CLI", id)
			}
			peer, err := s.fileSystem(ctx, aws.ToString(destination.FileSystemId))
			if err != nil {
				return nil, fmt.Errorf("inspect EFS replication destination: %w", err)
			}
			peers = append(peers, peer)
		}
		for _, peer := range peers {
			resource := efsResource(peer)
			if !s.selector.Matches(resource) {
				return nil, fmt.Errorf("EFS replication peer %s must also be explicitly selected", resource.ID)
			}
			if err := refuseIfProtected(resource.ID, resource.Tags); err != nil {
				return nil, err
			}
		}
	}
	return replications, nil
}

func (s *EFSService) Delete(ctx context.Context, resource Resource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.fileSystem(ctx, resource.ID)
	if err != nil {
		return err
	}
	if err := refuseIfProtected(resource.ID, efsResource(fs).Tags); err != nil {
		return err
	}
	replications, err := s.replication(ctx, resource.ID)
	if err != nil {
		return err
	}

	// Read and protect access points before making any destructive request.
	var accessPoints []types.AccessPointDescription
	accessPages := efs.NewDescribeAccessPointsPaginator(s.client, &efs.DescribeAccessPointsInput{FileSystemId: &resource.ID})
	for accessPages.HasMorePages() {
		page, err := accessPages.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, point := range page.AccessPoints {
			tags := make(map[string]string, len(point.Tags))
			for _, tag := range point.Tags {
				tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
			}
			if err := refuseIfProtected(aws.ToString(point.AccessPointId), tags); err != nil {
				return err
			}
			accessPoints = append(accessPoints, point)
		}
	}
	var targets []types.MountTargetDescription
	targetPages := efs.NewDescribeMountTargetsPaginator(s.client, &efs.DescribeMountTargetsInput{FileSystemId: &resource.ID})
	for targetPages.HasMorePages() {
		page, err := targetPages.NextPage(ctx)
		if err != nil {
			return err
		}
		targets = append(targets, page.MountTargets...)
	}

	for _, replication := range replications {
		_, err := s.client.DeleteReplicationConfiguration(ctx, &efs.DeleteReplicationConfigurationInput{
			SourceFileSystemId: replication.SourceFileSystemId, DeletionMode: types.DeletionModeAllConfigurations,
		})
		if err != nil {
			return err
		}
	}
	for _, replication := range replications {
		ids := []string{aws.ToString(replication.SourceFileSystemId)}
		for _, destination := range replication.Destinations {
			ids = append(ids, aws.ToString(destination.FileSystemId))
		}
		for _, id := range ids {
			if err := waitUntilGone(ctx, func(ctx context.Context) error {
				output, err := s.client.DescribeReplicationConfigurations(ctx, &efs.DescribeReplicationConfigurationsInput{FileSystemId: &id})
				var missing *types.ReplicationNotFound
				if errors.As(err, &missing) || (err == nil && len(output.Replications) == 0) {
					return errResourceGone
				}
				return err
			}); err != nil {
				return fmt.Errorf("wait for EFS replication removal on %s: %w", id, err)
			}
		}
	}
	for _, point := range accessPoints {
		if _, err := s.client.DeleteAccessPoint(ctx, &efs.DeleteAccessPointInput{AccessPointId: point.AccessPointId}); err != nil {
			return err
		}
	}
	for _, point := range accessPoints {
		if err := waitUntilGone(ctx, func(ctx context.Context) error {
			_, err := s.client.DescribeAccessPoints(ctx, &efs.DescribeAccessPointsInput{AccessPointId: point.AccessPointId})
			var missing *types.AccessPointNotFound
			if errors.As(err, &missing) {
				return errResourceGone
			}
			return err
		}); err != nil {
			return fmt.Errorf("wait for EFS access point removal: %w", err)
		}
	}
	for _, target := range targets {
		if target.LifeCycleState == types.LifeCycleStateDeleting {
			continue
		}
		if _, err := s.client.DeleteMountTarget(ctx, &efs.DeleteMountTargetInput{MountTargetId: target.MountTargetId}); err != nil {
			return err
		}
	}
	// Prerequisites must complete even with --wait=false.
	for _, target := range targets {
		if err := waitUntilGone(ctx, func(ctx context.Context) error {
			_, err := s.client.DescribeMountTargets(ctx, &efs.DescribeMountTargetsInput{MountTargetId: target.MountTargetId})
			var missing *types.MountTargetNotFound
			if errors.As(err, &missing) {
				return errResourceGone
			}
			return err
		}); err != nil {
			return fmt.Errorf("wait for EFS mount target removal: %w", err)
		}
	}
	_, err = s.client.DeleteFileSystem(ctx, &efs.DeleteFileSystemInput{FileSystemId: &resource.ID})
	return err
}

func (s *EFSService) Wait(ctx context.Context, resource Resource) error {
	return waitUntilGone(ctx, func(ctx context.Context) error {
		_, err := s.fileSystem(ctx, resource.ID)
		var missing *types.FileSystemNotFound
		if errors.As(err, &missing) {
			return errResourceGone
		}
		return err
	})
}
