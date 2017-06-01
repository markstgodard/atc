package worker_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"time"

	"code.cloudfoundry.org/clock/fakeclock"
	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/garden/gardenfakes"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	"github.com/concourse/atc"
	"github.com/concourse/atc/db/lock/lockfakes"
	"github.com/concourse/atc/dbng"
	"github.com/concourse/atc/dbng/dbngfakes"
	. "github.com/concourse/atc/worker"
	"github.com/concourse/atc/worker/workerfakes"
	"github.com/concourse/baggageclaim"
	"github.com/concourse/baggageclaim/baggageclaimfakes"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("ContainerProvider", func() {
	var (
		logger                    *lagertest.TestLogger
		fakeImageFetchingDelegate *workerfakes.FakeImageFetchingDelegate

		fakeCreatingContainer *dbngfakes.FakeCreatingContainer
		fakeCreatedContainer  *dbngfakes.FakeCreatedContainer

		fakeGardenClient            *gardenfakes.FakeClient
		fakeGardenContainer         *gardenfakes.FakeContainer
		fakeBaggageclaimClient      *baggageclaimfakes.FakeClient
		fakeVolumeClient            *workerfakes.FakeVolumeClient
		fakeImageFactory            *workerfakes.FakeImageFactory
		fakeImage                   *workerfakes.FakeImage
		fakeDBTeam                  *dbngfakes.FakeTeam
		fakeDBVolumeFactory         *dbngfakes.FakeVolumeFactory
		fakeDBResourceCacheFactory  *dbngfakes.FakeResourceCacheFactory
		fakeDBResourceConfigFactory *dbngfakes.FakeResourceConfigFactory
		fakeLockFactory             *lockfakes.FakeLockFactory
		fakeWorker                  *workerfakes.FakeWorker

		containerProvider        ContainerProvider
		containerProviderFactory ContainerProviderFactory

		fakeLocalInput    *workerfakes.FakeInputSource
		fakeRemoteInput   *workerfakes.FakeInputSource
		fakeRemoteInputAS *workerfakes.FakeArtifactSource

		fakeRemoteInputContainerVolume *workerfakes.FakeVolume
		fakeLocalVolume                *workerfakes.FakeVolume
		fakeOutputVolume               *workerfakes.FakeVolume
		fakeLocalCOWVolume             *workerfakes.FakeVolume
		fakeResourceCacheVolume        *workerfakes.FakeVolume

		cancel            <-chan os.Signal
		containerSpec     ContainerSpec
		resourceUser      dbng.ResourceUser
		containerMetadata dbng.ContainerMetadata
		resourceTypes     atc.VersionedResourceTypes

		findOrCreateErr       error
		findOrCreateContainer Container

		stubbedVolumes map[string]*workerfakes.FakeVolume
		volumeSpecs    map[string]VolumeSpec
	)

	disasterErr := errors.New("disaster")

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")

		fakeCreatingContainer = new(dbngfakes.FakeCreatingContainer)
		fakeCreatingContainer.HandleReturns("some-handle")
		fakeCreatedContainer = new(dbngfakes.FakeCreatedContainer)

		fakeImageFetchingDelegate = new(workerfakes.FakeImageFetchingDelegate)

		fakeGardenClient = new(gardenfakes.FakeClient)
		fakeBaggageclaimClient = new(baggageclaimfakes.FakeClient)
		fakeVolumeClient = new(workerfakes.FakeVolumeClient)
		fakeImageFactory = new(workerfakes.FakeImageFactory)
		fakeImage = new(workerfakes.FakeImage)
		fakeImage.FetchForContainerReturns(FetchedImage{
			Metadata: ImageMetadata{
				Env: []string{"IMAGE=ENV"},
			},
			URL: "some-image-url",
		}, nil)
		fakeImageFactory.GetImageReturns(fakeImage, nil)
		fakeLockFactory = new(lockfakes.FakeLockFactory)
		fakeWorker = new(workerfakes.FakeWorker)

		fakeDBTeamFactory := new(dbngfakes.FakeTeamFactory)
		fakeDBTeam = new(dbngfakes.FakeTeam)
		fakeDBTeamFactory.GetByIDReturns(fakeDBTeam)
		fakeDBVolumeFactory = new(dbngfakes.FakeVolumeFactory)
		fakeClock := fakeclock.NewFakeClock(time.Unix(0, 123))
		fakeDBResourceCacheFactory = new(dbngfakes.FakeResourceCacheFactory)
		fakeDBResourceConfigFactory = new(dbngfakes.FakeResourceConfigFactory)
		fakeGardenContainer = new(gardenfakes.FakeContainer)
		fakeGardenClient.CreateReturns(fakeGardenContainer, nil)

		containerProviderFactory = NewContainerProviderFactory(
			fakeGardenClient,
			fakeBaggageclaimClient,
			fakeVolumeClient,
			fakeImageFactory,
			fakeDBVolumeFactory,
			fakeDBResourceCacheFactory,
			fakeDBResourceConfigFactory,
			fakeDBTeamFactory,
			fakeLockFactory,
			"http://proxy.com",
			"https://proxy.com",
			"http://noproxy.com",
			fakeClock,
		)

		containerProvider = containerProviderFactory.ContainerProviderFor(fakeWorker)

		fakeLocalInput = new(workerfakes.FakeInputSource)
		fakeLocalInput.NameReturns("local-input")
		fakeLocalInput.DestinationPathReturns("/some/work-dir/local-input")
		fakeLocalInputAS := new(workerfakes.FakeArtifactSource)
		fakeLocalVolume = new(workerfakes.FakeVolume)
		fakeLocalVolume.PathReturns("/fake/local/volume")
		fakeLocalVolume.COWStrategyReturns(baggageclaim.COWStrategy{
			Parent: new(baggageclaimfakes.FakeVolume),
		})
		fakeLocalInputAS.VolumeOnReturns(fakeLocalVolume, true, nil)
		fakeLocalInput.SourceReturns(fakeLocalInputAS)

		fakeRemoteInput = new(workerfakes.FakeInputSource)
		fakeRemoteInput.NameReturns("remote-input")
		fakeRemoteInput.DestinationPathReturns("/some/work-dir/remote-input")
		fakeRemoteInputAS = new(workerfakes.FakeArtifactSource)
		fakeRemoteInputAS.VolumeOnReturns(nil, false, nil)
		fakeRemoteInput.SourceReturns(fakeRemoteInputAS)

		fakeScratchVolume := new(workerfakes.FakeVolume)
		fakeScratchVolume.PathReturns("/fake/scratch/volume")

		fakeWorkdirVolume := new(workerfakes.FakeVolume)
		fakeWorkdirVolume.PathReturns("/fake/work-dir/volume")

		fakeOutputVolume = new(workerfakes.FakeVolume)
		fakeOutputVolume.PathReturns("/fake/output/volume")

		fakeLocalCOWVolume = new(workerfakes.FakeVolume)
		fakeLocalCOWVolume.PathReturns("/fake/local/cow/volume")

		fakeRemoteInputContainerVolume = new(workerfakes.FakeVolume)
		fakeRemoteInputContainerVolume.PathReturns("/fake/remote/input/container/volume")

		fakeResourceCacheVolume = new(workerfakes.FakeVolume)
		fakeResourceCacheVolume.PathReturns("/fake/resource/cache/volume")

		stubbedVolumes = map[string]*workerfakes.FakeVolume{
			"/scratch":                    fakeScratchVolume,
			"/some/work-dir":              fakeWorkdirVolume,
			"/some/work-dir/local-input":  fakeLocalCOWVolume,
			"/some/work-dir/remote-input": fakeRemoteInputContainerVolume,
			"/some/work-dir/output":       fakeOutputVolume,
		}

		volumeSpecs = map[string]VolumeSpec{}

		fakeVolumeClient.FindOrCreateCOWVolumeForContainerStub = func(logger lager.Logger, volumeSpec VolumeSpec, creatingContainer dbng.CreatingContainer, volume Volume, teamID int, mountPath string) (Volume, error) {
			Expect(volume).To(Equal(fakeLocalVolume))

			volume, found := stubbedVolumes[mountPath]
			if !found {
				panic("unknown container volume: " + mountPath)
			}

			volumeSpecs[mountPath] = volumeSpec

			return volume, nil
		}

		fakeVolumeClient.FindOrCreateVolumeForContainerStub = func(logger lager.Logger, volumeSpec VolumeSpec, creatingContainer dbng.CreatingContainer, teamID int, mountPath string) (Volume, error) {
			volume, found := stubbedVolumes[mountPath]
			if !found {
				panic("unknown container volume: " + mountPath)
			}

			volumeSpecs[mountPath] = volumeSpec

			return volume, nil
		}

		cancel = make(chan os.Signal)

		resourceUser = dbng.ForBuild(42)

		containerMetadata = dbng.ContainerMetadata{
			StepName: "some-step",
		}

		containerSpec = ContainerSpec{
			TeamID: 73410,

			ImageSpec: ImageSpec{
				ImageResource: &atc.ImageResource{
					Type:   "docker-image",
					Source: atc.Source{"some": "image"},
				},
			},

			User: "some-user",
			Env:  []string{"SOME=ENV"},

			Dir: "/some/work-dir",

			Inputs: []InputSource{
				fakeLocalInput,
				fakeRemoteInput,
			},

			Outputs: OutputPaths{
				"some-output": "/some/work-dir/output",
			},

			ResourceCache: &VolumeMount{
				Volume:    fakeResourceCacheVolume,
				MountPath: "/some/resource/cache",
			},
		}

		resourceTypes = atc.VersionedResourceTypes{
			{
				ResourceType: atc.ResourceType{
					Type:   "some-type",
					Source: atc.Source{"some": "source"},
				},
				Version: atc.Version{"some": "version"},
			},
		}
	})

	ItHandlesContainerInCreatingState := func() {
		Context("when container exists in garden", func() {
			BeforeEach(func() {
				fakeGardenClient.LookupReturns(fakeGardenContainer, nil)
			})

			It("does not acquire lock", func() {
				Expect(fakeLockFactory.AcquireCallCount()).To(Equal(0))
			})

			It("marks container as created", func() {
				Expect(fakeCreatingContainer.CreatedCallCount()).To(Equal(1))
			})

			It("returns worker container", func() {
				Expect(findOrCreateContainer).ToNot(BeNil())
			})
		})

		Context("when container does not exist in garden", func() {
			BeforeEach(func() {
				fakeGardenClient.LookupReturns(nil, garden.ContainerNotFoundError{})
			})

			It("gets image", func() {
				Expect(fakeImageFactory.GetImageCallCount()).To(Equal(1))
				Expect(fakeImage.FetchForContainerCallCount()).To(Equal(1))
			})

			It("acquires lock", func() {
				Expect(fakeLockFactory.AcquireCallCount()).To(Equal(1))
			})

			It("creates container in garden", func() {
				Expect(fakeGardenClient.CreateCallCount()).To(Equal(1))
			})

			It("marks container as created", func() {
				Expect(fakeCreatingContainer.CreatedCallCount()).To(Equal(1))
			})

			It("returns worker container", func() {
				Expect(findOrCreateContainer).ToNot(BeNil())
			})

			Context("when failing to create container in garden", func() {
				BeforeEach(func() {
					fakeGardenClient.CreateReturns(nil, disasterErr)
				})

				It("returns an error", func() {
					Expect(findOrCreateErr).To(Equal(disasterErr))
				})

				It("does not mark container as created", func() {
					Expect(fakeCreatingContainer.CreatedCallCount()).To(Equal(0))
				})
			})

			Context("when getting image fails", func() {
				BeforeEach(func() {
					fakeImageFactory.GetImageReturns(nil, disasterErr)
				})

				It("returns an error", func() {
					Expect(findOrCreateErr).To(Equal(disasterErr))
				})

				It("does not create container in garden", func() {
					Expect(fakeGardenClient.CreateCallCount()).To(Equal(0))
				})
			})
		})
	}

	ItHandlesContainerInCreatedState := func() {
		Context("when container exists in garden", func() {
			BeforeEach(func() {
				fakeGardenClient.LookupReturns(fakeGardenContainer, nil)
			})

			It("returns container", func() {
				Expect(findOrCreateContainer).ToNot(BeNil())
			})
		})

		Context("when container does not exist in garden", func() {
			var containerNotFoundErr error

			BeforeEach(func() {
				containerNotFoundErr = garden.ContainerNotFoundError{}
				fakeGardenClient.LookupReturns(nil, containerNotFoundErr)
			})

			It("returns an error", func() {
				Expect(findOrCreateErr).To(Equal(containerNotFoundErr))
			})
		})
	}

	ItHandlesNonExistentContainer := func(createDatabaseCallCountFunc func() int) {
		It("gets image", func() {
			Expect(fakeImageFactory.GetImageCallCount()).To(Equal(1))
			_, actualWorker, actualVolumeClient, actualImageSpec, actualTeamID, actualCancel, actualDelegate, actualResourceUser, actualResourceTypes := fakeImageFactory.GetImageArgsForCall(0)
			Expect(actualWorker).To(Equal(fakeWorker))
			Expect(actualVolumeClient).To(Equal(fakeVolumeClient))
			Expect(actualImageSpec).To(Equal(containerSpec.ImageSpec))
			Expect(actualImageSpec).ToNot(BeZero())
			Expect(actualTeamID).To(Equal(containerSpec.TeamID))
			Expect(actualTeamID).ToNot(BeZero())
			Expect(actualCancel).To(Equal(cancel))
			Expect(actualDelegate).To(Equal(fakeImageFetchingDelegate))
			Expect(actualResourceUser).To(Equal(resourceUser))
			Expect(actualResourceTypes).To(Equal(resourceTypes))

			Expect(fakeImage.FetchForContainerCallCount()).To(Equal(1))
			_, actualContainer := fakeImage.FetchForContainerArgsForCall(0)
			Expect(actualContainer).To(Equal(fakeCreatingContainer))
		})

		It("creates container in database", func() {
			Expect(createDatabaseCallCountFunc()).To(Equal(1))
		})

		It("acquires lock", func() {
			Expect(fakeLockFactory.AcquireCallCount()).To(Equal(1))
		})

		It("creates the container in garden", func() {
			Expect(fakeGardenClient.CreateCallCount()).To(Equal(1))

			actualSpec := fakeGardenClient.CreateArgsForCall(0)
			Expect(actualSpec).To(Equal(garden.ContainerSpec{
				Handle:     "some-handle",
				RootFSPath: "some-image-url",
				Properties: garden.Properties{"user": "some-user"},
				BindMounts: []garden.BindMount{
					{
						SrcPath: "/fake/scratch/volume",
						DstPath: "/scratch",
						Mode:    garden.BindMountModeRW,
					},
					{
						SrcPath: "/fake/work-dir/volume",
						DstPath: "/some/work-dir",
						Mode:    garden.BindMountModeRW,
					},
					{
						SrcPath: "/fake/local/cow/volume",
						DstPath: "/some/work-dir/local-input",
						Mode:    garden.BindMountModeRW,
					},
					{
						SrcPath: "/fake/remote/input/container/volume",
						DstPath: "/some/work-dir/remote-input",
						Mode:    garden.BindMountModeRW,
					},
					{
						SrcPath: "/fake/output/volume",
						DstPath: "/some/work-dir/output",
						Mode:    garden.BindMountModeRW,
					},
					{
						SrcPath: "/fake/resource/cache/volume",
						DstPath: "/some/resource/cache",
						Mode:    garden.BindMountModeRW,
					},
				},
				Env: []string{
					"IMAGE=ENV",
					"SOME=ENV",
					"http_proxy=http://proxy.com",
					"https_proxy=https://proxy.com",
					"no_proxy=http://noproxy.com",
				},
			}))
		})

		It("creates each volume unprivileged", func() {
			Expect(volumeSpecs).To(Equal(map[string]VolumeSpec{
				"/scratch":                    VolumeSpec{Strategy: baggageclaim.EmptyStrategy{}},
				"/some/work-dir":              VolumeSpec{Strategy: baggageclaim.EmptyStrategy{}},
				"/some/work-dir/output":       VolumeSpec{Strategy: baggageclaim.EmptyStrategy{}},
				"/some/work-dir/local-input":  VolumeSpec{Strategy: fakeLocalVolume.COWStrategy()},
				"/some/work-dir/remote-input": VolumeSpec{Strategy: baggageclaim.EmptyStrategy{}},
			}))
		})

		It("streams remote inputs into newly created container volumes", func() {
			Expect(fakeRemoteInputAS.StreamToCallCount()).To(Equal(1))
			ad := fakeRemoteInputAS.StreamToArgsForCall(0)

			err := ad.StreamIn(".", bytes.NewBufferString("some-stream"))
			Expect(err).ToNot(HaveOccurred())

			Expect(fakeRemoteInputContainerVolume.StreamInCallCount()).To(Equal(1))

			dst, from := fakeRemoteInputContainerVolume.StreamInArgsForCall(0)
			Expect(dst).To(Equal("."))
			Expect(ioutil.ReadAll(from)).To(Equal([]byte("some-stream")))
		})

		It("marks container as created", func() {
			Expect(fakeCreatingContainer.CreatedCallCount()).To(Equal(1))
		})

		Context("when the fetched image was privileged", func() {
			BeforeEach(func() {
				fakeImage.FetchForContainerReturns(FetchedImage{
					Privileged: true,
					Metadata: ImageMetadata{
						Env: []string{"IMAGE=ENV"},
					},
					URL: "some-image-url",
				}, nil)
			})

			It("creates the container privileged", func() {
				Expect(fakeGardenClient.CreateCallCount()).To(Equal(1))

				actualSpec := fakeGardenClient.CreateArgsForCall(0)
				Expect(actualSpec.Privileged).To(BeTrue())
			})

			It("creates each volume privileged", func() {
				Expect(volumeSpecs).To(Equal(map[string]VolumeSpec{
					"/scratch":                    VolumeSpec{Privileged: true, Strategy: baggageclaim.EmptyStrategy{}},
					"/some/work-dir":              VolumeSpec{Privileged: true, Strategy: baggageclaim.EmptyStrategy{}},
					"/some/work-dir/output":       VolumeSpec{Privileged: true, Strategy: baggageclaim.EmptyStrategy{}},
					"/some/work-dir/local-input":  VolumeSpec{Privileged: true, Strategy: fakeLocalVolume.COWStrategy()},
					"/some/work-dir/remote-input": VolumeSpec{Privileged: true, Strategy: baggageclaim.EmptyStrategy{}},
				}))
			})

		})

		Context("when an input has the path set to the workdir itself", func() {
			BeforeEach(func() {
				fakeLocalInput.DestinationPathReturns("/some/work-dir")
				delete(stubbedVolumes, "/some/work-dir/local-input")
				stubbedVolumes["/some/work-dir"] = fakeLocalCOWVolume
			})

			It("does not create or mount a work-dir, as we support this for backwards-compatibility", func() {
				Expect(fakeGardenClient.CreateCallCount()).To(Equal(1))

				actualSpec := fakeGardenClient.CreateArgsForCall(0)
				Expect(actualSpec.BindMounts).To(Equal([]garden.BindMount{
					{
						SrcPath: "/fake/scratch/volume",
						DstPath: "/scratch",
						Mode:    garden.BindMountModeRW,
					},
					{
						SrcPath: "/fake/local/cow/volume",
						DstPath: "/some/work-dir",
						Mode:    garden.BindMountModeRW,
					},
					{
						SrcPath: "/fake/remote/input/container/volume",
						DstPath: "/some/work-dir/remote-input",
						Mode:    garden.BindMountModeRW,
					},
					{
						SrcPath: "/fake/output/volume",
						DstPath: "/some/work-dir/output",
						Mode:    garden.BindMountModeRW,
					},
					{
						SrcPath: "/fake/resource/cache/volume",
						DstPath: "/some/resource/cache",
						Mode:    garden.BindMountModeRW,
					},
				}))
			})
		})

		Context("when getting image fails", func() {
			BeforeEach(func() {
				fakeImageFactory.GetImageReturns(nil, disasterErr)
			})

			It("returns an error", func() {
				Expect(findOrCreateErr).To(Equal(disasterErr))
			})

			It("does not create container in database", func() {
				Expect(createDatabaseCallCountFunc()).To(Equal(0))
			})

			It("does not create container in garden", func() {
				Expect(fakeGardenClient.CreateCallCount()).To(Equal(0))
			})
		})

		Context("when failing to create container in garden", func() {
			BeforeEach(func() {
				fakeGardenClient.CreateReturns(nil, disasterErr)
			})

			It("returns an error", func() {
				Expect(findOrCreateErr).To(Equal(disasterErr))
			})

			It("does not mark container as created", func() {
				Expect(fakeCreatingContainer.CreatedCallCount()).To(Equal(0))
			})
		})
	}

	Describe("FindOrCreateBuildContainer", func() {
		BeforeEach(func() {
			fakeDBTeam.CreateBuildContainerReturns(fakeCreatingContainer, nil)
			fakeLockFactory.AcquireReturns(new(lockfakes.FakeLock), true, nil)
		})

		JustBeforeEach(func() {
			findOrCreateContainer, findOrCreateErr = containerProvider.FindOrCreateBuildContainer(
				logger,
				cancel,
				fakeImageFetchingDelegate,
				42,
				atc.PlanID("some-plan-id"),
				containerMetadata,
				containerSpec,
				resourceTypes,
			)
		})

		Context("when container exists in database in creating state", func() {
			BeforeEach(func() {
				fakeDBTeam.FindBuildContainerOnWorkerReturns(fakeCreatingContainer, nil, nil)
			})

			ItHandlesContainerInCreatingState()
		})

		Context("when container exists in database in created state", func() {
			BeforeEach(func() {
				fakeDBTeam.FindBuildContainerOnWorkerReturns(nil, fakeCreatedContainer, nil)
			})

			ItHandlesContainerInCreatedState()
		})

		Context("when container does not exist in database", func() {
			BeforeEach(func() {
				fakeDBTeam.FindBuildContainerOnWorkerReturns(nil, nil, nil)
			})

			ItHandlesNonExistentContainer(func() int {
				return fakeDBTeam.CreateBuildContainerCallCount()
			})
		})
	})

	Describe("FindOrCreateResourceCheckContainer", func() {
		BeforeEach(func() {
			fakeDBResourceConfigFactory.FindOrCreateResourceConfigReturns(&dbng.UsedResourceConfig{
				ID: 42,
			}, nil)
			fakeDBTeam.CreateResourceCheckContainerReturns(fakeCreatingContainer, nil)
			fakeLockFactory.AcquireReturns(new(lockfakes.FakeLock), true, nil)
		})

		JustBeforeEach(func() {
			findOrCreateContainer, findOrCreateErr = containerProvider.FindOrCreateResourceCheckContainer(
				logger,
				resourceUser,
				cancel,
				fakeImageFetchingDelegate,
				containerMetadata,
				containerSpec,
				resourceTypes,
				"some-resource",
				atc.Source{"some": "source"},
			)
		})

		Context("when container exists in database in creating state", func() {
			BeforeEach(func() {
				fakeDBTeam.FindResourceCheckContainerOnWorkerReturns(fakeCreatingContainer, nil, nil)
			})

			ItHandlesContainerInCreatingState()
		})

		Context("when container exists in database in created state", func() {
			BeforeEach(func() {
				fakeDBTeam.FindResourceCheckContainerOnWorkerReturns(nil, fakeCreatedContainer, nil)
			})

			ItHandlesContainerInCreatedState()
		})

		Context("when container does not exist in database", func() {
			BeforeEach(func() {
				fakeDBTeam.FindResourceCheckContainerOnWorkerReturns(nil, nil, nil)
			})

			ItHandlesNonExistentContainer(func() int {
				return fakeDBTeam.CreateResourceCheckContainerCallCount()
			})
		})
	})

	Describe("CreateResourceGetContainer", func() {
		BeforeEach(func() {
			fakeDBTeam.CreateResourceGetContainerReturns(fakeCreatingContainer, nil)
			fakeLockFactory.AcquireReturns(new(lockfakes.FakeLock), true, nil)
		})

		JustBeforeEach(func() {
			findOrCreateContainer, findOrCreateErr = containerProvider.CreateResourceGetContainer(
				logger,
				dbng.ForBuild(42),
				cancel,
				fakeImageFetchingDelegate,
				containerMetadata,
				containerSpec,
				resourceTypes,
				"some-resource",
				atc.Version{"some": "version"},
				atc.Source{"some": "source"},
				atc.Params{},
			)
		})

		ItHandlesNonExistentContainer(func() int {
			return fakeDBTeam.CreateResourceGetContainerCallCount()
		})
	})

	Describe("FindCreatedContainerByHandle", func() {
		var (
			foundContainer Container
			findErr        error
			found          bool
		)

		JustBeforeEach(func() {
			foundContainer, found, findErr = containerProvider.FindCreatedContainerByHandle(logger, "some-container-handle", 42)
		})

		Context("when the gardenClient returns a container and no error", func() {
			var (
				fakeContainer *gardenfakes.FakeContainer
			)

			BeforeEach(func() {
				fakeContainer = new(gardenfakes.FakeContainer)
				fakeContainer.HandleReturns("provider-handle")

				fakeDBVolumeFactory.FindVolumesForContainerReturns([]dbng.CreatedVolume{}, nil)

				fakeDBTeam.FindCreatedContainerByHandleReturns(fakeCreatedContainer, true, nil)
				fakeGardenClient.LookupReturns(fakeContainer, nil)
			})

			It("returns the container", func() {
				Expect(findErr).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				Expect(foundContainer.Handle()).To(Equal(fakeContainer.Handle()))
			})

			Describe("the found container", func() {
				It("can be destroyed", func() {
					err := foundContainer.Destroy()
					Expect(err).NotTo(HaveOccurred())

					By("destroying via garden")
					Expect(fakeGardenClient.DestroyCallCount()).To(Equal(1))
					Expect(fakeGardenClient.DestroyArgsForCall(0)).To(Equal("provider-handle"))
				})
			})

			Context("when the concourse:volumes property is present", func() {
				var (
					handle1Volume         *baggageclaimfakes.FakeVolume
					handle2Volume         *baggageclaimfakes.FakeVolume
					expectedHandle1Volume Volume
					expectedHandle2Volume Volume
				)

				BeforeEach(func() {
					handle1Volume = new(baggageclaimfakes.FakeVolume)
					handle2Volume = new(baggageclaimfakes.FakeVolume)

					fakeVolume1 := new(dbngfakes.FakeCreatedVolume)
					fakeVolume2 := new(dbngfakes.FakeCreatedVolume)

					expectedHandle1Volume = NewVolume(handle1Volume, fakeVolume1)
					expectedHandle2Volume = NewVolume(handle2Volume, fakeVolume2)

					fakeVolume1.HandleReturns("handle-1")
					fakeVolume2.HandleReturns("handle-2")

					fakeVolume1.PathReturns("/handle-1/path")
					fakeVolume2.PathReturns("/handle-2/path")

					fakeDBVolumeFactory.FindVolumesForContainerReturns([]dbng.CreatedVolume{fakeVolume1, fakeVolume2}, nil)

					fakeBaggageclaimClient.LookupVolumeStub = func(logger lager.Logger, handle string) (baggageclaim.Volume, bool, error) {
						if handle == "handle-1" {
							return handle1Volume, true, nil
						} else if handle == "handle-2" {
							return handle2Volume, true, nil
						} else {
							panic("unknown handle: " + handle)
						}
					}
				})

				Describe("VolumeMounts", func() {
					It("returns all bound volumes based on properties on the container", func() {
						Expect(foundContainer.VolumeMounts()).To(ConsistOf([]VolumeMount{
							{Volume: expectedHandle1Volume, MountPath: "/handle-1/path"},
							{Volume: expectedHandle2Volume, MountPath: "/handle-2/path"},
						}))
					})

					Context("when LookupVolume returns an error", func() {
						disaster := errors.New("nope")

						BeforeEach(func() {
							fakeBaggageclaimClient.LookupVolumeReturns(nil, false, disaster)
						})

						It("returns the error on lookup", func() {
							Expect(findErr).To(Equal(disaster))
						})
					})
				})
			})

			Context("when the user property is present", func() {
				var (
					actualSpec garden.ProcessSpec
					actualIO   garden.ProcessIO
				)

				BeforeEach(func() {
					actualSpec = garden.ProcessSpec{
						Path: "some-path",
						Args: []string{"some", "args"},
						Env:  []string{"some=env"},
						Dir:  "some-dir",
					}

					actualIO = garden.ProcessIO{}

					fakeContainer.PropertiesReturns(garden.Properties{"user": "maverick"}, nil)
				})

				JustBeforeEach(func() {
					foundContainer.Run(actualSpec, actualIO)
				})

				Describe("Run", func() {
					It("calls Run() on the garden container and injects the user", func() {
						Expect(fakeContainer.RunCallCount()).To(Equal(1))
						spec, io := fakeContainer.RunArgsForCall(0)
						Expect(spec).To(Equal(garden.ProcessSpec{
							Path: "some-path",
							Args: []string{"some", "args"},
							Env:  []string{"some=env"},
							Dir:  "some-dir",
							User: "maverick",
						}))
						Expect(io).To(Equal(garden.ProcessIO{}))
					})
				})
			})

			Context("when the user property is not present", func() {
				var (
					actualSpec garden.ProcessSpec
					actualIO   garden.ProcessIO
				)

				BeforeEach(func() {
					actualSpec = garden.ProcessSpec{
						Path: "some-path",
						Args: []string{"some", "args"},
						Env:  []string{"some=env"},
						Dir:  "some-dir",
					}

					actualIO = garden.ProcessIO{}

					fakeContainer.PropertiesReturns(garden.Properties{"user": ""}, nil)
				})

				JustBeforeEach(func() {
					foundContainer.Run(actualSpec, actualIO)
				})

				Describe("Run", func() {
					It("calls Run() on the garden container and injects the default user", func() {
						Expect(fakeContainer.RunCallCount()).To(Equal(1))
						spec, io := fakeContainer.RunArgsForCall(0)
						Expect(spec).To(Equal(garden.ProcessSpec{
							Path: "some-path",
							Args: []string{"some", "args"},
							Env:  []string{"some=env"},
							Dir:  "some-dir",
							User: "root",
						}))
						Expect(io).To(Equal(garden.ProcessIO{}))
						Expect(fakeContainer.RunCallCount()).To(Equal(1))
					})
				})
			})
		})

		Context("when the gardenClient returns garden.ContainerNotFoundError", func() {
			BeforeEach(func() {
				fakeGardenClient.LookupReturns(nil, garden.ContainerNotFoundError{Handle: "some-handle"})
			})

			It("returns false and no error", func() {
				Expect(findErr).ToNot(HaveOccurred())
				Expect(found).To(BeFalse())
			})
		})

		Context("when the gardenClient returns an error", func() {
			var expectedErr error

			BeforeEach(func() {
				expectedErr = fmt.Errorf("container not found")
				fakeGardenClient.LookupReturns(nil, expectedErr)
			})

			It("returns nil and forwards the error", func() {
				Expect(findErr).To(Equal(expectedErr))

				Expect(foundContainer).To(BeNil())
			})
		})

	})

})
