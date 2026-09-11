package nuke

import (
	"context"
	"fmt"
	"strings"
)

type Resource struct {
	Service string            `json:"service"`
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Name    string            `json:"name,omitempty"`
	ARN     string            `json:"arn,omitempty"`
	Tags    map[string]string `json:"tags,omitempty"`
	Status  string            `json:"status"`
	Error   string            `json:"error,omitempty"`
}

type Service interface {
	Name() string
	Discover(context.Context) ([]Resource, error)
	Delete(context.Context, Resource) error
	Wait(context.Context, Resource) error
}

type Selector struct {
	IDs  []string
	Tags map[string]string
}

func (s Selector) Matches(resource Resource) bool {
	for _, id := range s.IDs {
		if id == resource.ID || id == resource.Name || id == resource.ARN {
			return true
		}
	}

	if len(s.Tags) == 0 {
		return false
	}
	for key, value := range s.Tags {
		if resource.Tags[key] != value {
			return false
		}
	}
	return true
}

func IsProtected(resource Resource) bool {
	return strings.EqualFold(resource.Tags["DoNotDelete"], "true")
}

func refuseIfProtected(name string, tags map[string]string) error {
	if IsProtected(Resource{Tags: tags}) {
		return fmt.Errorf("resource %s is tagged DoNotDelete=true", name)
	}
	return nil
}
