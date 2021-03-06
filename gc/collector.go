package gc

import "code.cloudfoundry.org/lager"

//go:generate counterfeiter . Collector

type Collector interface {
	Run() error
}

type aggregateCollector struct {
	logger                     lager.Logger
	buildCollector             Collector
	workerCollector            Collector
	resourceCacheUseCollector  Collector
	resourceConfigUseCollector Collector
	resourceConfigCollector    Collector
	resourceCacheCollector     Collector
	volumeCollector            Collector
	containerCollector         Collector
}

func NewCollector(
	logger lager.Logger,
	buildCollector Collector,
	workers Collector,
	resourceCacheUses Collector,
	resourceConfigUses Collector,
	resourceConfigs Collector,
	resourceCaches Collector,
	volumes Collector,
	containers Collector,
) Collector {
	return &aggregateCollector{
		logger:                     logger,
		buildCollector:             buildCollector,
		workerCollector:            workers,
		resourceCacheUseCollector:  resourceCacheUses,
		resourceConfigUseCollector: resourceConfigUses,
		resourceConfigCollector:    resourceConfigs,
		resourceCacheCollector:     resourceCaches,
		volumeCollector:            volumes,
		containerCollector:         containers,
	}
}

func (c *aggregateCollector) Run() error {
	var err error

	err = c.buildCollector.Run()
	if err != nil {
		c.logger.Error("failed-to-run-build-collector", err)
	}

	err = c.workerCollector.Run()
	if err != nil {
		c.logger.Error("failed-to-run-worker-collector", err)
	}

	err = c.resourceCacheUseCollector.Run()
	if err != nil {
		c.logger.Error("failed-to-run-resource-cache-use-collector", err)
	}

	err = c.resourceConfigUseCollector.Run()
	if err != nil {
		c.logger.Error("failed-to-run-resource-config-use-collector", err)
	}

	err = c.resourceConfigCollector.Run()
	if err != nil {
		c.logger.Error("failed-to-run-resource-config-collector", err)
	}

	err = c.resourceCacheCollector.Run()
	if err != nil {
		c.logger.Error("failed-to-run-resource-cache-collector", err)
	}

	err = c.containerCollector.Run()
	if err != nil {
		c.logger.Error("container-collector", err)
	}

	err = c.volumeCollector.Run()
	if err != nil {
		c.logger.Error("volume-collector", err)
	}

	return nil
}
