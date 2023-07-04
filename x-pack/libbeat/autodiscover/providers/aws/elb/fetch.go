// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package elb

import (
	"context"
	"sync"
	"time"
	"math"


	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"go.uber.org/multierr"
	"github.com/aws/aws-sdk-go/aws/awserr"

	"github.com/elastic/elastic-agent-libs/logp"
	"golang.org/x/sync/semaphore"
)



var (
	maxAPICalls   = 3 // Set this to a suitable value based on AWS limits.
	apiSemaphore = semaphore.NewWeighted(int64(maxAPICalls))
)
// fetcher is an interface that can fetch a list of lbListener (load balancer + listener) objects without pagination being necessary.
type fetcher interface {
	fetch(ctx context.Context) ([]*lbListener, error)
}

// apiMultiFetcher fetches results from multiple clients concatenating their results together
// Useful since we have a fetcher per region, this combines them.
type apiMultiFetcher struct {
	fetchers []fetcher
}

func (amf *apiMultiFetcher) fetch(ctx context.Context) ([]*lbListener, error) {
	fetchResults := make(chan []*lbListener)
	fetchErr := make(chan error)

	// Simultaneously fetch all from each region
	for _, f := range amf.fetchers {
		go func(f fetcher) {
			fres, ferr := f.fetch(ctx)
			if ferr != nil {
				fetchErr <- ferr
			} else {
				fetchResults <- fres
			}
		}(f)
	}

	var results []*lbListener
	var errs []error

	for pending := len(amf.fetchers); pending > 0; pending-- {
		select {
		case r := <-fetchResults:
			results = append(results, r...)
		case e := <-fetchErr:
			errs = append(errs, e)
		}
	}

	return results, multierr.Combine(errs...)
}

// apiFetcher is a concrete implementation of fetcher that hits the real AWS API.
type apiFetcher struct {
	client autodiscoverElbClient
}

type autodiscoverElbClient interface {
	elasticloadbalancingv2.DescribeListenersAPIClient
	elasticloadbalancingv2.DescribeLoadBalancersAPIClient
}

func newAPIFetcher(clients []autodiscoverElbClient) fetcher {
	fetchers := make([]fetcher, len(clients))
	for idx, client := range clients {
		fetchers[idx] = &apiFetcher{
			client: client,
		}
	}
	return &apiMultiFetcher{fetchers}
}

// fetch attempts to request the full list of lbListener objects.
// It accomplishes this by fetching a page of load balancers, then one go routine
// per listener API request. Each page of results has O(n)+1 perf since we need that
// additional fetch per lb. We let the goroutine scheduler sort things out, and use
// a sync.Pool to limit the number of in-flight requests.
func (f *apiFetcher) fetch(ctx context.Context) ([]*lbListener, error) {
	var pageSize int32 = 50

	ctx, cancel := context.WithCancel(ctx)
	ir := &fetchRequest{
		paginator: elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(f.client,
			&elasticloadbalancingv2.DescribeLoadBalancersInput{PageSize: &pageSize}),
		client:   f.client,
		taskPool: sync.Pool{},
		context:  ctx,
		cancel:   cancel,
		logger:   logp.NewLogger("autodiscover-elb-fetch"),
	}

	// Limit concurrency against the AWS API by creating a pool of objects
	// This is hard coded for now. The concurrency limit of 10 was set semi-arbitrarily.
	for i := 0; i < 10; i++ {
		ir.taskPool.Put(nil)
	}

	return ir.fetch()
}

// fetchRequest provides a way to get all pages from a
// elbv2.DescribeLoadBalancersPager and all listeners for the given LoadBalancers.
type fetchRequest struct {
	paginator    *elasticloadbalancingv2.DescribeLoadBalancersPaginator
	client       elasticloadbalancingv2.DescribeListenersAPIClient
	lbListeners  []*lbListener
	errs         []error
	resultsLock  sync.Mutex
	taskPool     sync.Pool
	pendingTasks sync.WaitGroup
	context      context.Context
	cancel       func()
	logger       *logp.Logger
    totalELBs    int
}

const maxWaitTime = 60 // Maximum wait time is 60 seconds

func backoff(attempt int) time.Duration {
	minTime := float64(1)
	maxTime := float64(60)
	factor := math.Pow(2, float64(attempt))
	sleep := math.Min(minTime*factor, maxTime)
	return time.Duration(sleep) * time.Second
}


func (p *fetchRequest) fetch() ([]*lbListener, error) {
	p.dispatch(p.fetchAllPages)

	// Only fetch future pages when there are no longer requests in-flight from a previous page
	p.pendingTasks.Wait()

	// Acquire the results lock to ensure memory
	// consistency between the last write and this read
	p.resultsLock.Lock()
	defer p.resultsLock.Unlock()
    // Retrieve the total number of items from the first page
	// Since everything is async we have to retrieve any errors that occurred from here
	if len(p.errs) > 0 {
		return nil, multierr.Combine(p.errs...)
	}

	return p.lbListeners, nil
}

func (p *fetchRequest) fetchAllPages() {
	// Keep fetching pages unless we're stopped OR there are no pages left
	for {
		select {
		case <-p.context.Done():
			p.logger.Debug("done fetching ELB pages, context cancelled")
			return
		default:
			if !p.paginator.HasMorePages() {
				p.logger.Debug("fetched all ELB pages")
				return
			}
			p.fetchNextPage()
			p.logger.Debug("fetched ELB page")
		}
	}
}

func (p *fetchRequest) fetchNextPage() {
	page, err := p.paginator.NextPage(p.context)
	if err != nil {
		p.recordErrResult(err)
		return
	}

	if page.LoadBalancers == nil {
		p.logger.Error("LoadBalancers in fetched page is nil")
		return
	}

	p.logger.Info("Fetching next page of listeners")
	p.totalELBs += len(page.LoadBalancers)
	p.logger.Info("Total ELBs left to be processed: ", p.totalELBs)

	for _, lb := range page.LoadBalancers {
		p.dispatch(func() { p.fetchListeners(lb) })
	}

	p.totalELBs -= len(page.LoadBalancers)
	p.logger.Info("Total ELBs left to be processed: ", p.totalELBs)
}

// dispatch runs the given func in a new goroutine, properly throttling requests
// with the taskPool and also managing the pendingTasks waitGroup to ensure all
// results are accumulated.
func (p *fetchRequest) dispatch(fn func()) {
	p.pendingTasks.Add(1)

	go func() {
		slot := p.taskPool.Get()
		defer p.taskPool.Put(slot)
		defer p.pendingTasks.Done()

		fn()
	}()
}

func (p *fetchRequest) fetchListeners(lb types.LoadBalancer) {
	describeListenersInput := &elasticloadbalancingv2.DescribeListenersInput{LoadBalancerArn: lb.LoadBalancerArn}
	paginator := elasticloadbalancingv2.NewDescribeListenersPaginator(p.client, describeListenersInput)

	attempt := 1

	for {
		select {
		case <-p.context.Done():
			p.logger.Debug("Context done, stopping listener fetch")
			return
		default:
			if !paginator.HasMorePages() {
				p.logger.Debug("No more pages, stopping listener fetch")
				// Decrement totalELBs and log it here
				p.totalELBs--
				p.logger.Info("Total ELBs left to be processed: ", p.totalELBs)
				return
			}

			// Acquire a semaphore slot before making an API call
			if err := apiSemaphore.Acquire(p.context, 1); err != nil {
				p.logger.Error("Failed to acquire semaphore:", err)
				return
			}

			p.logger.Debug("Fetching next page of listeners")
			page, err := paginator.NextPage(p.context)
			if err != nil {
				apiSemaphore.Release(1) // Make sure to release the semaphore in case of an error
				if aerr, ok := err.(awserr.Error); ok {
					switch aerr.Code() {
					case "ThrottlingException":
						// We got throttled, apply exponential backoff
						waitTime := backoff(attempt)
						time.Sleep(waitTime)
						attempt++
						continue
					}
				}

				p.logger.Errorf("Error fetching page: %v", err)
				p.recordErrResult(err)
				return
			}

			apiSemaphore.Release(1) // Release the semaphore after a successful API call

			if page == nil {
				p.logger.Error("Fetched page is nil")
				return
			}

			if page.Listeners == nil {
				p.logger.Error("Listeners in fetched page is nil")
				return
			}

			for i := range page.Listeners {
				p.logger.Debugf("Recording listener: %v", page.Listeners[i])
				p.recordGoodResult(&lb, &page.Listeners[i])
			}
		}
	}
}





func (p *fetchRequest) recordGoodResult(lb *types.LoadBalancer, lbl *types.Listener) {
	p.resultsLock.Lock()
	defer p.resultsLock.Unlock()

	p.lbListeners = append(p.lbListeners, &lbListener{lb, lbl})
}

func (p *fetchRequest) recordErrResult(err error) {
	p.resultsLock.Lock()
	defer p.resultsLock.Unlock()

	p.errs = append(p.errs, err)

	p.cancel()
}
