package pkgmirror

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/boltdb/bolt"
	"github.com/rande/goapp"
)

var (
	SyncInProgressError = errors.New("A synchronization is already running")
	EmptyKeyError       = errors.New("No value available")
)

type PackagistConfig struct {
	Server string
	Code   []byte
	Path   string
}

func NewPackagistService() *PackagistService {
	return &PackagistService{
		Config: &PackagistConfig{
			Server: "https://packagist.org",
			Code:   []byte("packagist"),
			Path:   "./data/composer",
		},
	}
}

type PackagistService struct {
	DB              *bolt.DB
	Config          *PackagistConfig
	DownloadManager *DownloadManager
	Logger          *log.Entry
	GitConfig       *GitConfig
	lock            bool
}

func (ps *PackagistService) Init(app *goapp.App) error {
	var err error

	ps.Logger.Info("Init")

	ps.DB, err = bolt.Open(fmt.Sprintf("%s/%s.db", ps.Config.Path, ps.Config.Code), 0600, &bolt.Options{
		Timeout:  1 * time.Second,
		ReadOnly: false,
	})

	if err != nil {
		return err
	}

	return ps.DB.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(ps.Config.Code)

		return err
	})
}

func (ps *PackagistService) Serve(state *goapp.GoroutineState) error {
	ps.Logger.Info("Starting Packagist Service")

	for {
		ps.SyncPackages()
		ps.UpdateEntryPoints()
		ps.CleanPackages()

		ps.Logger.Info("Wait before starting a new sync...")
		time.Sleep(10 * time.Second)
	}
}

func (ps *PackagistService) End() error {
	return nil
}

func (ps *PackagistService) SyncPackages() error {
	logger := ps.Logger.WithFields(log.Fields{
		"action": "SyncPackages",
	})

	logger.Info("Starting SyncPackages")

	dm := &DownloadManager{
		Add:   make(chan PackageInformation),
		Count: 5,
		Done:  make(chan struct{}),
	}
	pr := &PackagesResult{}

	if err := LoadRemoteStruct(fmt.Sprintf("%s/packages.json", ps.Config.Server), pr); err != nil {
		logger.WithFields(log.Fields{
			"path":  "packages.json",
			"error": err.Error(),
		}).Error("Error loading packages.json")

		return err // an error occurs avoid empty file
	}

	var wg sync.WaitGroup
	var lock sync.Mutex

	PackageListener := make(chan PackageInformation)

	go dm.Wait(func(id int, pkgs <-chan PackageInformation) {
		for pkg := range pkgs {
			p := &PackageResult{}

			cpt := 0
			for {
				if err := LoadRemoteStruct(fmt.Sprintf("%s/p/%s", ps.Config.Server, pkg.GetSourceKey()), p); err != nil {
					logger.WithFields(log.Fields{
						"package": pkg.Package,
						"error":   err.Error(),
					}).Error("Error loading package information")

					cpt++

					if cpt > 5 {
						break
					}
				} else {
					break
				}
			}

			pkg.PackageResult = *p

			PackageListener <- pkg
		}
	})

	go func() {
		for {
			select {
			case pkg, valid := <-PackageListener:

				if !valid {
					logger.Info("PackageListener is closed")
					return
				}

				lock.Lock()
				ps.savePackage(&pkg)
				lock.Unlock()

				wg.Done()
			}
		}
	}()

	for provider, sha := range pr.ProviderIncludes {
		path := strings.Replace(provider, "%hash%", sha.Sha256, -1)

		logger := logger.WithFields(log.Fields{
			"provider": provider,
			"hash":     sha.Sha256,
		})

		logger.Info("Loading provider information")

		pr := &ProvidersResult{}

		if err := LoadRemoteStruct(fmt.Sprintf("%s/%s", ps.Config.Server, path), pr); err != nil {
			logger.WithField("error", err.Error()).Error("Error loading provider information")
		} else {
			logger.Debug("End loading provider information")
		}

		for name, sha := range pr.Providers {
			p := PackageInformation{
				Server:  string(ps.Config.Code),
				Package: name,
				Exist:   false,
			}

			logger := logger.WithFields(log.Fields{
				"package": name,
			})

			lock.Lock()
			ps.DB.View(func(tx *bolt.Tx) error {
				b := tx.Bucket(ps.Config.Code)
				data := b.Get([]byte(p.Package))

				p.Exist = false

				if err := json.Unmarshal(data, &p); err == nil {
					p.Exist = p.HashSource == sha.Sha256
				}

				p.HashSource = sha.Sha256

				return nil
			})
			lock.Unlock()

			if !p.Exist {
				logger.Info("Add new package")

				wg.Add(1)
				dm.Add <- p
			} else {
				logger.Debug("Skipping package")
			}
		}
	}

	logger.Info("Wait for download to complete")

	wg.Wait()

	close(PackageListener)
	dm.Done <- struct{}{}

	return nil
}

func (ps *PackagistService) Get(key string) ([]byte, error) {
	var data []byte

	err := ps.DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(ps.Config.Code)

		raw := b.Get([]byte(key))

		if len(raw) == 0 {
			return EmptyKeyError
		}

		data = make([]byte, len(raw))

		copy(data, raw)

		return nil
	})

	return data, err
}

// This method generates the different entry points required by a repository.
//
func (ps *PackagistService) UpdateEntryPoints() error {

	if ps.lock {
		return SyncInProgressError
	}

	ps.lock = true

	defer func() {
		ps.lock = false
	}()

	logger := ps.Logger.WithFields(log.Fields{
		"action": "UpdateEntryPoints",
	})

	logger.Info("Start")

	pkgResult := &PackagesResult{}
	if err := LoadRemoteStruct(fmt.Sprintf("%s/packages.json", ps.Config.Server), pkgResult); err != nil {
		logger.WithFields(log.Fields{
			"path":  "packages.json",
			"error": err.Error(),
		}).Error("Error loading packages.json")

		return err // an error occurs avoid empty file
	}

	logger.Info("packages.json loaded")

	providers := map[string]*ProvidersResult{}

	for provider, sha := range pkgResult.ProviderIncludes {
		pr := &ProvidersResult{}

		logger.WithField("provider", provider).Info("Load provider")

		if err := LoadRemoteStruct(fmt.Sprintf("%s/%s", ps.Config.Server, strings.Replace(provider, "%hash%", sha.Sha256, -1)), pr); err != nil {
			ps.Logger.WithField("error", err.Error()).Error("Error loading provider information")
		}

		providers[provider] = pr

		// iterate packages from each provider
		for name := range pr.Providers {
			ps.DB.View(func(tx *bolt.Tx) error {
				b := tx.Bucket(ps.Config.Code)
				data := b.Get([]byte(name))

				pi := &PackageInformation{}
				if err := json.Unmarshal(data, pi); err != nil {
					return err
				}

				// https://github.com/golang/go/issues/3117
				p := providers[provider].Providers[name]
				p.Sha256 = pi.HashTarget
				providers[provider].Providers[name] = p

				return nil
			})
		}

		// save provider file
		data, err := json.Marshal(providers[provider])

		if err != nil {
			ps.Logger.WithFields(log.Fields{
				"provider": provider,
				"error":    err,
			}).Error("Unable to marshal provider information")
		}

		hash := sha256.Sum256(data)

		// https://github.com/golang/go/issues/3117
		p := pkgResult.ProviderIncludes[provider]
		p.Sha256 = hex.EncodeToString(hash[:])
		pkgResult.ProviderIncludes[provider] = p

		path := fmt.Sprintf("%s", strings.Replace(provider, "%hash%", p.Sha256, -1))

		ps.DB.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(ps.Config.Code)
			b.Put([]byte(path), data)

			ps.Logger.WithFields(log.Fields{
				"provider": provider,
				"path":     path,
			}).Debug("Save provider")

			return nil
		})
	}

	//pr.ProviderIncludes = providerIncludes
	pkgResult.ProvidersURL = fmt.Sprintf("/%s%s", ps.Config.Code, pkgResult.ProvidersURL)
	pkgResult.Notify = fmt.Sprintf("/%s%s", ps.Config.Code, pkgResult.Notify)
	pkgResult.NotifyBatch = fmt.Sprintf("/%s%s", ps.Config.Code, pkgResult.NotifyBatch)
	pkgResult.Search = fmt.Sprintf("/%s%s", ps.Config.Code, pkgResult.Search)

	ps.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(ps.Config.Code)
		data, _ := json.Marshal(pkgResult)
		b.Put([]byte("packages.json"), data)

		ps.Logger.Info("Save packages.json")

		return nil
	})

	ps.Logger.Info("End UpdateEntryPoints")

	return nil
}

func (ps *PackagistService) UpdatePackage(name string) error {
	if ps.lock {
		return SyncInProgressError
	}

	if i := strings.Index(name, "$"); i > 0 {
		name = name[:i]
	}

	pkg := &PackageInformation{
		Package: name,
		Server:  ps.Config.Server,
	}

	ps.Logger.WithFields(log.Fields{
		"package": pkg.Package,
		"action":  "UpdatePackage",
	}).Info("Explicit reload package information")

	err := ps.DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(ps.Config.Code)
		data := b.Get([]byte(pkg.Package))

		if err := json.Unmarshal(data, &pkg); err == nil {
			return err
		}

		return nil
	})

	if err != nil {
		return err // unknown package
	}

	pkg.PackageResult = PackageResult{}

	if err := LoadRemoteStruct(fmt.Sprintf("%s/p/%s", ps.Config.Server, pkg.GetSourceKey()), &pkg.PackageResult); err != nil {
		ps.Logger.WithFields(log.Fields{
			"package": pkg.Package,
			"error":   err.Error(),
			"action":  "UpdatePackage",
		}).Error("Error loading package information")
	}

	if err := ps.savePackage(pkg); err != nil {
		return err
	}

	return ps.UpdateEntryPoints()
}

func (ps *PackagistService) savePackage(pkg *PackageInformation) error {
	return ps.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(ps.Config.Code)

		logger := ps.Logger.WithFields(log.Fields{
			"package": pkg.Package,
			"path":    pkg.GetTargetKey(),
		})

		for _, version := range pkg.PackageResult.Packages[pkg.Package] {
			version.Dist.URL = GitRewriteArchive(ps.GitConfig, version.Dist.URL)
			version.Source.URL = GitRewriteRepository(ps.GitConfig, version.Source.URL)
		}

		// compute hash
		data, _ := json.Marshal(pkg.PackageResult)
		sha := sha256.Sum256(data)
		pkg.HashTarget = hex.EncodeToString(sha[:])

		// compress data for saving bytes ...
		buf := bytes.NewBuffer([]byte(""))
		if gz, err := gzip.NewWriterLevel(buf, gzip.BestCompression); err != nil {
			logger.WithError(err).Error("Error while creating gzip writer")
		} else {
			if _, err := gz.Write(data); err != nil {
				logger.WithError(err).Error("Error while writing gzip")
			}

			gz.Close()
		}

		// store the path
		if err := b.Put([]byte(pkg.GetTargetKey()), buf.Bytes()); err != nil {
			logger.WithError(err).Error("Error updating/creating definition")

			return err
		} else {
			data, _ := json.Marshal(pkg)

			if err := b.Put([]byte(pkg.Package), data); err != nil {
				logger.WithError(err).Error("Error updating/creating hash definition")

				return err
			}
		}

		return nil
	})
}

func (ps *PackagistService) CleanPackages() error {

	logger := ps.Logger.WithFields(log.Fields{
		"action": "CleanPackages",
	})

	logger.Info("Start cleaning ...")

	ps.DB.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket(ps.Config.Code)

		pkgResult := &PackagesResult{}
		if data, err := ps.Get("packages.json"); err != nil {
			logger.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("Error loading packages.json")

			return err // an error occurs avoid empty file
		} else {
			json.Unmarshal(data, pkgResult)
		}

		var pi *PackageInformation

		b.ForEach(func(k, v []byte) error {
			name := string(k)
			if i := strings.Index(name, "$"); i > 0 {

				if name[0:10] == "p/provider" {
					// skipping
					for provider, sha := range pkgResult.ProviderIncludes {
						if name[0:i+1] == provider[0:i+1] && name[i+1:len(name)-5] != sha.Sha256 {
							logger.WithFields(log.Fields{
								"package":      provider[0:i],
								"hash_target":  sha.Sha256,
								"hash_current": name[i+1 : len(name)-5],
							}).Info("Delete provider definition")

							b.Delete(k)
						}
					}

				} else if name[0:i] == pi.Package {

					if pi.HashTarget != name[i+1:] {
						logger.WithFields(log.Fields{
							"package":      name,
							"hash_target":  pi.HashTarget,
							"hash_current": name[i+1:],
						}).Info("Delete package definition")

						b.Delete(k)
					}
				} else {
					logger.WithField("package", name).Error("Orphan reference")
				}
			} else {
				pi = &PackageInformation{}

				json.Unmarshal(v, pi)
			}

			return nil
		})

		return nil
	})

	return nil
}
