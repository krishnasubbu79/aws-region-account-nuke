package nuke

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Engine struct {
	Services    []Service
	Selector    Selector
	Parallel    int
	Wait        bool
	WaitTimeout time.Duration
}

func (e Engine) Plan(ctx context.Context) ([]Resource, error) {
	var plan []Resource
	for _, service := range e.Services {
		resources, err := service.Discover(ctx)
		if err != nil {
			return nil, fmt.Errorf("discover %s resources: %w", service.Name(), err)
		}
		for _, resource := range resources {
			if !e.Selector.Matches(resource) {
				continue
			}
			if IsProtected(resource) {
				resource.Status = "protected"
			} else {
				resource.Status = "planned"
			}
			plan = append(plan, resource)
		}
	}
	return plan, nil
}

func (e Engine) Execute(ctx context.Context, plan []Resource) []Resource {
	result := append([]Resource(nil), plan...)
	workers := e.Parallel
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				e.executeOne(ctx, &result[index])
			}
		}()
	}
	for index := range result {
		if result[index].Status == "planned" {
			jobs <- index
		}
	}
	close(jobs)
	wg.Wait()
	return result
}

func (e Engine) executeOne(ctx context.Context, resource *Resource) {
	service := e.service(resource.Service)
	if service == nil {
		resource.Status = "failed"
		resource.Error = "service implementation not found"
		return
	}
	operationContext, cancel := context.WithTimeout(ctx, e.WaitTimeout)
	defer cancel()
	if err := service.Delete(operationContext, *resource); err != nil {
		resource.Status = "failed"
		resource.Error = err.Error()
		return
	}
	resource.Status = "delete-submitted"
	if !e.Wait {
		return
	}

	if err := service.Wait(operationContext, *resource); err != nil {
		resource.Status = "failed"
		resource.Error = "wait for deletion: " + err.Error()
		return
	}
	resource.Status = "deleted"
}

func (e Engine) service(name string) Service {
	for _, service := range e.Services {
		if service.Name() == name {
			return service
		}
	}
	return nil
}
