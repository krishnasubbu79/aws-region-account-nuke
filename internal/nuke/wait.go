package nuke

import (
	"context"
	"errors"
	"time"

	"github.com/aws/smithy-go"
)

var errResourceGone = errors.New("resource no longer exists")

func waitUntilGone(ctx context.Context, check func(context.Context) error) error {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		err := check(ctx)
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errResourceGone) {
		return true
	}
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	switch apiError.ErrorCode() {
	case "NotFoundException", "ResourceNotFoundException",
		"DBClusterNotFound", "DBClusterNotFoundFault", "DBInstanceNotFound", "DBInstanceNotFoundFault",
		"CacheClusterNotFound", "CacheClusterNotFoundFault", "ReplicationGroupNotFound", "ReplicationGroupNotFoundFault",
		"ServerlessCacheNotFoundFault":
		return true
	default:
		return false
	}
}
