package worker

import (
	"fmt"
	"os"
	"time"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/lager"
	"github.com/concourse/atc"
	"github.com/concourse/atc/db/lock"
	"github.com/concourse/atc/dbng"
	"github.com/concourse/baggageclaim"
)

const creatingContainerRetryDelay = 1 * time.Second

//go:generate counterfeiter . ContainerProviderFactory

type ContainerProviderFactory interface {
	ContainerProviderFor(Worker) ContainerProvider
}

type containerProviderFactory struct {
	gardenClient            garden.Client
	baggageclaimClient      baggageclaim.Client
	volumeClient            VolumeClient
	imageFactory            ImageFactory
	dbVolumeFactory         dbng.VolumeFactory
	dbResourceCacheFactory  dbng.ResourceCacheFactory
	dbResourceConfigFactory dbng.ResourceConfigFactory
	dbTeamFactory           dbng.TeamFactory

	lockFactory lock.LockFactory

	httpProxyURL  string
	httpsProxyURL string
	noProxy       string

	clock clock.Clock
}

func NewContainerProviderFactory(
	gardenClient garden.Client,
	baggageclaimClient baggageclaim.Client,
	volumeClient VolumeClient,
	imageFactory ImageFactory,
	dbVolumeFactory dbng.VolumeFactory,
	dbResourceCacheFactory dbng.ResourceCacheFactory,
	dbResourceConfigFactory dbng.ResourceConfigFactory,
	dbTeamFactory dbng.TeamFactory,
	lockFactory lock.LockFactory,
	httpProxyURL string,
	httpsProxyURL string,
	noProxy string,
	clock clock.Clock,
) ContainerProviderFactory {
	return &containerProviderFactory{
		gardenClient:            gardenClient,
		baggageclaimClient:      baggageclaimClient,
		volumeClient:            volumeClient,
		imageFactory:            imageFactory,
		dbVolumeFactory:         dbVolumeFactory,
		dbResourceCacheFactory:  dbResourceCacheFactory,
		dbResourceConfigFactory: dbResourceConfigFactory,
		dbTeamFactory:           dbTeamFactory,
		lockFactory:             lockFactory,
		httpProxyURL:            httpProxyURL,
		httpsProxyURL:           httpsProxyURL,
		noProxy:                 noProxy,
		clock:                   clock,
	}
}

func (f *containerProviderFactory) ContainerProviderFor(worker Worker) ContainerProvider {
	return &containerProvider{
		gardenClient:            f.gardenClient,
		baggageclaimClient:      f.baggageclaimClient,
		volumeClient:            f.volumeClient,
		imageFactory:            f.imageFactory,
		dbVolumeFactory:         f.dbVolumeFactory,
		dbResourceCacheFactory:  f.dbResourceCacheFactory,
		dbResourceConfigFactory: f.dbResourceConfigFactory,
		dbTeamFactory:           f.dbTeamFactory,
		lockFactory:             f.lockFactory,
		httpProxyURL:            f.httpProxyURL,
		httpsProxyURL:           f.httpsProxyURL,
		noProxy:                 f.noProxy,
		clock:                   f.clock,
		worker:                  worker,
	}
}

//go:generate counterfeiter . ContainerProvider

type ContainerProvider interface {
	FindCreatedContainerByHandle(
		logger lager.Logger,
		handle string,
		teamID int,
	) (Container, bool, error)

	FindOrCreateBuildContainer(
		logger lager.Logger,
		cancel <-chan os.Signal,
		delegate ImageFetchingDelegate,
		buildID int,
		planID atc.PlanID,
		metadata dbng.ContainerMetadata,
		spec ContainerSpec,
		resourceTypes atc.VersionedResourceTypes,
	) (Container, error)

	FindOrCreateResourceCheckContainer(
		logger lager.Logger,
		resourceUser dbng.ResourceUser,
		cancel <-chan os.Signal,
		delegate ImageFetchingDelegate,
		metadata dbng.ContainerMetadata,
		spec ContainerSpec,
		resourceTypes atc.VersionedResourceTypes,
		resourceType string,
		source atc.Source,
	) (Container, error)

	CreateResourceGetContainer(
		logger lager.Logger,
		resourceUser dbng.ResourceUser,
		cancel <-chan os.Signal,
		delegate ImageFetchingDelegate,
		metadata dbng.ContainerMetadata,
		spec ContainerSpec,
		resourceTypes atc.VersionedResourceTypes,
		resourceTypeName string,
		version atc.Version,
		source atc.Source,
		params atc.Params,
	) (Container, error)
}

type containerProvider struct {
	gardenClient            garden.Client
	baggageclaimClient      baggageclaim.Client
	volumeClient            VolumeClient
	imageFactory            ImageFactory
	dbVolumeFactory         dbng.VolumeFactory
	dbResourceCacheFactory  dbng.ResourceCacheFactory
	dbResourceConfigFactory dbng.ResourceConfigFactory
	dbTeamFactory           dbng.TeamFactory

	lockFactory lock.LockFactory
	provider    WorkerProvider

	worker        Worker
	httpProxyURL  string
	httpsProxyURL string
	noProxy       string

	clock clock.Clock
}

func (p *containerProvider) FindOrCreateBuildContainer(
	logger lager.Logger,
	cancel <-chan os.Signal,
	delegate ImageFetchingDelegate,
	buildID int,
	planID atc.PlanID,
	metadata dbng.ContainerMetadata,
	spec ContainerSpec,
	resourceTypes atc.VersionedResourceTypes,
) (Container, error) {
	return p.findOrCreateContainer(
		logger,
		dbng.ForBuild(buildID),
		cancel,
		delegate,
		spec,
		resourceTypes,
		func() (dbng.CreatingContainer, dbng.CreatedContainer, error) {
			return p.dbTeamFactory.GetByID(spec.TeamID).FindBuildContainerOnWorker(
				p.worker.Name(),
				buildID,
				planID,
			)
		},
		func() (dbng.CreatingContainer, error) {
			return p.dbTeamFactory.GetByID(spec.TeamID).CreateBuildContainer(
				p.worker.Name(),
				buildID,
				planID,
				metadata,
			)
		},
	)
}

func (p *containerProvider) FindOrCreateResourceCheckContainer(
	logger lager.Logger,
	resourceUser dbng.ResourceUser,
	cancel <-chan os.Signal,
	delegate ImageFetchingDelegate,
	metadata dbng.ContainerMetadata,
	spec ContainerSpec,
	resourceTypes atc.VersionedResourceTypes,
	resourceType string,
	source atc.Source,
) (Container, error) {
	resourceConfig, err := p.dbResourceConfigFactory.FindOrCreateResourceConfig(
		logger,
		resourceUser,
		resourceType,
		source,
		resourceTypes,
	)
	if err != nil {
		logger.Error("failed-to-get-resource-config", err)
		return nil, err
	}

	return p.findOrCreateContainer(
		logger,
		resourceUser,
		cancel,
		delegate,
		spec,
		resourceTypes,
		func() (dbng.CreatingContainer, dbng.CreatedContainer, error) {
			logger.Debug("looking-for-container-in-db", lager.Data{
				"team-id":            spec.TeamID,
				"worker-name":        p.worker.Name(),
				"resource-config-id": resourceConfig.ID,
			})
			return p.dbTeamFactory.GetByID(spec.TeamID).FindResourceCheckContainerOnWorker(
				p.worker.Name(),
				resourceConfig,
			)
		},
		func() (dbng.CreatingContainer, error) {
			logger.Debug("creating-container-in-db", lager.Data{
				"team-id":            spec.TeamID,
				"worker-name":        p.worker.Name(),
				"resource-config-id": resourceConfig.ID,
			})
			return p.dbTeamFactory.GetByID(spec.TeamID).CreateResourceCheckContainer(
				p.worker.Name(),
				resourceConfig,
				metadata,
			)
		},
	)
}

func (p *containerProvider) CreateResourceGetContainer(
	logger lager.Logger,
	resourceUser dbng.ResourceUser,
	cancel <-chan os.Signal,
	delegate ImageFetchingDelegate,
	metadata dbng.ContainerMetadata,
	spec ContainerSpec,
	resourceTypes atc.VersionedResourceTypes,
	resourceTypeName string,
	version atc.Version,
	source atc.Source,
	params atc.Params,
) (Container, error) {
	resourceCache, err := p.dbResourceCacheFactory.FindOrCreateResourceCache(
		logger,
		resourceUser,
		resourceTypeName,
		version,
		source,
		params,
		resourceTypes,
	)
	if err != nil {
		logger.Error("failed-to-get-resource-cache", err, lager.Data{"user": resourceUser})
		return nil, err
	}

	return p.findOrCreateContainer(
		logger,
		resourceUser,
		cancel,
		delegate,
		spec,
		resourceTypes,
		func() (dbng.CreatingContainer, dbng.CreatedContainer, error) {
			return nil, nil, nil
		},
		func() (dbng.CreatingContainer, error) {
			return p.dbTeamFactory.GetByID(spec.TeamID).CreateResourceGetContainer(
				p.worker.Name(),
				resourceCache,
				metadata,
			)
		},
	)
}

func (p *containerProvider) FindCreatedContainerByHandle(
	logger lager.Logger,
	handle string,
	teamID int,
) (Container, bool, error) {
	gardenContainer, err := p.gardenClient.Lookup(handle)
	if err != nil {
		if _, ok := err.(garden.ContainerNotFoundError); ok {
			logger.Info("container-not-found")
			return nil, false, nil
		}

		logger.Error("failed-to-lookup-on-garden", err)
		return nil, false, err
	}

	createdContainer, found, err := p.dbTeamFactory.GetByID(teamID).FindCreatedContainerByHandle(handle)
	if err != nil {
		logger.Error("failed-to-lookup-in-db", err)
		return nil, false, err
	}

	if !found {
		return nil, false, nil
	}

	createdVolumes, err := p.dbVolumeFactory.FindVolumesForContainer(createdContainer)
	if err != nil {
		return nil, false, err
	}

	container, err := newGardenWorkerContainer(
		logger,
		gardenContainer,
		createdContainer,
		createdVolumes,
		p.gardenClient,
		p.baggageclaimClient,
		p.worker.Name(),
	)

	if err != nil {
		logger.Error("failed-to-construct-container", err)
		return nil, false, err
	}

	return container, true, nil
}

func (p *containerProvider) findOrCreateContainer(
	logger lager.Logger,
	resourceUser dbng.ResourceUser,
	cancel <-chan os.Signal,
	delegate ImageFetchingDelegate,
	spec ContainerSpec,
	resourceTypes atc.VersionedResourceTypes,
	findContainerFunc func() (dbng.CreatingContainer, dbng.CreatedContainer, error),
	createContainerFunc func() (dbng.CreatingContainer, error),
) (Container, error) {
	for {
		var gardenContainer garden.Container

		creatingContainer, createdContainer, err := findContainerFunc()
		if err != nil {
			logger.Error("failed-to-find-container-in-db", err)
			return nil, err
		}

		if createdContainer != nil {
			logger = logger.WithData(lager.Data{"container": createdContainer.Handle()})

			logger.Debug("found-created-container-in-db")

			gardenContainer, err = p.gardenClient.Lookup(createdContainer.Handle())
			if err != nil {
				logger.Error("failed-to-lookup-created-container-in-garden", err)
				return nil, err
			}

			return p.constructGardenWorkerContainer(
				logger,
				createdContainer,
				gardenContainer,
			)
		}

		if creatingContainer != nil {
			logger = logger.WithData(lager.Data{"container": creatingContainer.Handle()})

			logger.Debug("found-creating-container-in-db")

			gardenContainer, err = p.gardenClient.Lookup(creatingContainer.Handle())
			if err != nil {
				if _, ok := err.(garden.ContainerNotFoundError); !ok {
					logger.Error("failed-to-lookup-creating-container-in-garden", err)
					return nil, err
				}
			}

		}

		if gardenContainer != nil {
			logger.Debug("found-created-container-in-garden")
		} else {
			image, err := p.imageFactory.GetImage(
				logger,
				p.worker,
				p.volumeClient,
				spec.ImageSpec,
				spec.TeamID,
				cancel,
				delegate,
				resourceUser,
				resourceTypes,
			)
			if err != nil {
				return nil, err
			}

			if creatingContainer == nil {
				logger.Debug("creating-container-in-db")

				creatingContainer, err = createContainerFunc()
				if err != nil {
					logger.Error("failed-to-create-container-in-db", err)
					return nil, err
				}

				logger = logger.WithData(lager.Data{"container": creatingContainer.Handle()})

				logger.Debug("created-creating-container-in-db")
			}

			lock, acquired, err := p.lockFactory.Acquire(logger, lock.NewContainerCreatingLockID(creatingContainer.ID()))
			if err != nil {
				logger.Error("failed-to-acquire-container-creating-lock", err)
				return nil, err
			}

			if !acquired {
				p.clock.Sleep(creatingContainerRetryDelay)
				continue
			}

			defer lock.Release()

			logger.Debug("fetching-image")

			fetchedImage, err := image.FetchForContainer(logger, creatingContainer)
			if err != nil {
				logger.Error("failed-to-fetch-image-for-container", err)
				return nil, err
			}

			logger.Debug("creating-container-in-garden")

			gardenContainer, err = p.createGardenContainer(
				logger,
				creatingContainer,
				spec,
				fetchedImage,
			)
			if err != nil {
				logger.Error("failed-to-create-container-in-garden", err)
				return nil, err
			}

			logger.Debug("created-container-in-garden")
		}

		createdContainer, err = creatingContainer.Created()
		if err != nil {
			logger.Error("failed-to-mark-container-as-created", err)
			return nil, err
		}

		logger.Debug("created-container-in-db")

		return p.constructGardenWorkerContainer(
			logger,
			createdContainer,
			gardenContainer,
		)
	}
}

func (p *containerProvider) constructGardenWorkerContainer(
	logger lager.Logger,
	createdContainer dbng.CreatedContainer,
	gardenContainer garden.Container,
) (Container, error) {
	createdVolumes, err := p.dbVolumeFactory.FindVolumesForContainer(createdContainer)
	if err != nil {
		logger.Error("failed-to-find-container-volumes", err)
		return nil, err
	}

	return newGardenWorkerContainer(
		logger,
		gardenContainer,
		createdContainer,
		createdVolumes,
		p.gardenClient,
		p.baggageclaimClient,
		p.worker.Name(),
	)
}

func (p *containerProvider) createGardenContainer(
	logger lager.Logger,
	creatingContainer dbng.CreatingContainer,
	spec ContainerSpec,
	fetchedImage FetchedImage,
) (garden.Container, error) {
	volumeMounts := []VolumeMount{}

	scratchVolume, err := p.volumeClient.FindOrCreateVolumeForContainer(
		logger,
		VolumeSpec{
			Strategy:   baggageclaim.EmptyStrategy{},
			Privileged: fetchedImage.Privileged,
		},
		creatingContainer,
		spec.TeamID,
		"/scratch",
	)
	if err != nil {
		return nil, err
	}

	volumeMounts = append(volumeMounts, VolumeMount{
		Volume:    scratchVolume,
		MountPath: "/scratch",
	})

	if spec.Dir != "" && !p.anyMountTo(spec.Dir, spec.Inputs) {
		workdirVolume, volumeErr := p.volumeClient.FindOrCreateVolumeForContainer(
			logger,
			VolumeSpec{
				Strategy:   baggageclaim.EmptyStrategy{},
				Privileged: fetchedImage.Privileged,
			},
			creatingContainer,
			spec.TeamID,
			spec.Dir,
		)
		if volumeErr != nil {
			return nil, volumeErr
		}

		volumeMounts = append(volumeMounts, VolumeMount{
			Volume:    workdirVolume,
			MountPath: spec.Dir,
		})
	}

	for _, inputSource := range spec.Inputs {
		var inputVolume Volume

		localVolume, found, err := inputSource.Source().VolumeOn(p.worker)
		if err != nil {
			return nil, err
		}

		if found {
			inputVolume, err = p.volumeClient.FindOrCreateCOWVolumeForContainer(
				logger,
				VolumeSpec{
					Strategy:   localVolume.COWStrategy(),
					Privileged: fetchedImage.Privileged,
				},
				creatingContainer,
				localVolume,
				spec.TeamID,
				inputSource.DestinationPath(),
			)
			if err != nil {
				return nil, err
			}
		} else {
			inputVolume, err = p.volumeClient.FindOrCreateVolumeForContainer(
				logger,
				VolumeSpec{
					Strategy:   baggageclaim.EmptyStrategy{},
					Privileged: fetchedImage.Privileged,
				},
				creatingContainer,
				spec.TeamID,
				inputSource.DestinationPath(),
			)
			if err != nil {
				return nil, err
			}

			err = inputSource.Source().StreamTo(inputVolume)
			if err != nil {
				return nil, err
			}
		}

		volumeMounts = append(volumeMounts, VolumeMount{
			Volume:    inputVolume,
			MountPath: inputSource.DestinationPath(),
		})
	}

	for _, outputPath := range spec.Outputs {
		outVolume, volumeErr := p.volumeClient.FindOrCreateVolumeForContainer(
			logger,
			VolumeSpec{
				Strategy:   baggageclaim.EmptyStrategy{},
				Privileged: fetchedImage.Privileged,
			},
			creatingContainer,
			spec.TeamID,
			outputPath,
		)
		if volumeErr != nil {
			return nil, volumeErr
		}

		volumeMounts = append(volumeMounts, VolumeMount{
			Volume:    outVolume,
			MountPath: outputPath,
		})
	}

	if spec.ResourceCache != nil {
		volumeMounts = append(volumeMounts, *spec.ResourceCache)
	}

	bindMounts := []garden.BindMount{}

	volumeHandleMounts := map[string]string{}
	for _, mount := range volumeMounts {
		bindMounts = append(bindMounts, garden.BindMount{
			SrcPath: mount.Volume.Path(),
			DstPath: mount.MountPath,
			Mode:    garden.BindMountModeRW,
		})
		volumeHandleMounts[mount.Volume.Handle()] = mount.MountPath
	}

	gardenProperties := garden.Properties{}

	if spec.User != "" {
		gardenProperties[userPropertyName] = spec.User
	} else {
		gardenProperties[userPropertyName] = fetchedImage.Metadata.User
	}

	env := append(fetchedImage.Metadata.Env, spec.Env...)

	if p.httpProxyURL != "" {
		env = append(env, fmt.Sprintf("http_proxy=%s", p.httpProxyURL))
	}

	if p.httpsProxyURL != "" {
		env = append(env, fmt.Sprintf("https_proxy=%s", p.httpsProxyURL))
	}

	if p.noProxy != "" {
		env = append(env, fmt.Sprintf("no_proxy=%s", p.noProxy))
	}

	return p.gardenClient.Create(garden.ContainerSpec{
		Handle:     creatingContainer.Handle(),
		RootFSPath: fetchedImage.URL,
		Privileged: fetchedImage.Privileged,
		BindMounts: bindMounts,
		Env:        env,
		Properties: gardenProperties,
	})
}

func (p *containerProvider) anyMountTo(path string, inputs []InputSource) bool {
	for _, input := range inputs {
		if input.DestinationPath() == path {
			return true
		}
	}

	return false
}
