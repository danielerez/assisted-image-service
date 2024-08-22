package isoeditor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	log "github.com/sirupsen/logrus"
)

const (
	RamDiskPaddingLength = uint64(1024 * 1024) // 1MB
	ramDiskImagePath     = "/images/assisted_installer_custom.img"
	nmstateDiskImagePath = "/images/nmstate.img"
)

//go:generate mockgen -package=isoeditor -destination=mock_editor.go . Editor
type Editor interface {
	CreateMinimalISOTemplate(fullISOPath, rootFSURL, arch, minimalISOPath string) error
}

type rhcosEditor struct {
	workDir string
}

func NewEditor(dataDir string) Editor {
	return &rhcosEditor{workDir: dataDir}
}

func createNmstateRamDisk() ([]byte, error) {
	srcPath := "/usr/bin/nmstatectl"

	// Check if the file exists
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return nil, err
	}

	nmstateBinContent, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, err
	}

	compressedCpio, err := generateCompressedCPIO(nmstateBinContent, srcPath, 0o100_755)
	if err != nil {
		return nil, err
	}
	return compressedCpio, nil
}

// CreateMinimalISO Creates the minimal iso by removing the rootfs and adding the url
func CreateMinimalISO(extractDir, volumeID, rootFSURL, arch, minimalISOPath string) error {
	if err := os.Remove(filepath.Join(extractDir, "images/pxeboot/rootfs.img")); err != nil {
		return err
	}

	runtimeArch := normalizeCPUArchitecture(runtime.GOARCH)
	if runtimeArch == arch {
		// create a compressed nmstatectl binary RAM disk image
		compressedBuffer, err := createNmstateRamDisk()
		if err != nil {
			return err
		}

		f, err := os.Create(filepath.Join(extractDir, nmstateDiskImagePath))
		if err != nil {
			return err
		}

		_, err = f.Write(compressedBuffer)
		if err != nil {
			return err
		}
	}
	if err := embedInitrdPlaceholders(extractDir); err != nil {
		log.WithError(err).Warnf("Failed to embed initrd placeholders")
		return err
	}

	if err := fixGrubConfig(rootFSURL, extractDir); err != nil {
		log.WithError(err).Warnf("Failed to edit grub config")
		return err
	}

	// ignore isolinux.cfg for ppc64le because it doesn't exist
	if arch != "ppc64le" {
		if err := fixIsolinuxConfig(rootFSURL, extractDir); err != nil {
			log.WithError(err).Warnf("Failed to edit isolinux config")
			return err
		}
	}

	if err := Create(minimalISOPath, extractDir, volumeID); err != nil {
		return err
	}
	return nil
}

// CreateMinimalISOTemplate Creates the template minimal iso by removing the rootfs and adding the url
func (e *rhcosEditor) CreateMinimalISOTemplate(fullISOPath, rootFSURL, arch, minimalISOPath string) error {
	extractDir, err := os.MkdirTemp(e.workDir, "isoutil")
	if err != nil {
		return err
	}

	if err = Extract(fullISOPath, extractDir); err != nil {
		return err
	}

	volumeID, err := VolumeIdentifier(fullISOPath)
	if err != nil {
		return err
	}

	err = CreateMinimalISO(extractDir, volumeID, rootFSURL, arch, minimalISOPath)
	if err != nil {
		return err
	}

	return nil
}

func embedInitrdPlaceholders(extractDir string) error {
	f, err := os.Create(filepath.Join(extractDir, ramDiskImagePath))
	if err != nil {
		return err
	}
	defer func() {
		if deferErr := f.Sync(); deferErr != nil {
			log.WithError(deferErr).Error("Failed to sync disk image placeholder file")
		}
		if deferErr := f.Close(); deferErr != nil {
			log.WithError(deferErr).Error("Failed to close disk image placeholder file")
		}
	}()

	err = f.Truncate(int64(RamDiskPaddingLength))
	if err != nil {
		return err
	}

	return nil
}

func fixGrubConfig(rootFSURL, extractDir string) error {
	availableGrubPaths := []string{"EFI/redhat/grub.cfg", "EFI/fedora/grub.cfg", "boot/grub/grub.cfg", "EFI/centos/grub.cfg"}
	var foundGrubPath string
	for _, pathSection := range availableGrubPaths {
		path := filepath.Join(extractDir, pathSection)
		if _, err := os.Stat(path); err == nil {
			foundGrubPath = path
			break
		}
	}
	if len(foundGrubPath) == 0 {
		return fmt.Errorf("no grub.cfg found, possible paths are %v", availableGrubPaths)
	}

	// Add the rootfs url
	replacement := fmt.Sprintf("$1 $2 'coreos.live.rootfs_url=%s'", rootFSURL)
	if err := editFile(foundGrubPath, `(?m)^(\s+linux) (.+| )+$`, replacement); err != nil {
		return err
	}

	// Remove the coreos.liveiso parameter
	if err := editFile(foundGrubPath, ` coreos.liveiso=\S+`, ""); err != nil {
		return err
	}

	// Edit config to add custom ramdisk image to initrd
	if err := editFile(foundGrubPath, `(?m)^(\s+initrd) (.+| )+$`, fmt.Sprintf("$1 $2 %s %s", ramDiskImagePath, nmstateDiskImagePath)); err != nil {
		return err
	}

	return nil
}

func fixIsolinuxConfig(rootFSURL, extractDir string) error {
	replacement := fmt.Sprintf("$1 $2 coreos.live.rootfs_url=%s", rootFSURL)
	if err := editFile(filepath.Join(extractDir, "isolinux/isolinux.cfg"), `(?m)^(\s+append) (.+| )+$`, replacement); err != nil {
		return err
	}

	if err := editFile(filepath.Join(extractDir, "isolinux/isolinux.cfg"), ` coreos.liveiso=\S+`, ""); err != nil {
		return err
	}

	if err := editFile(filepath.Join(extractDir, "isolinux/isolinux.cfg"), `(?m)^(\s+append.*initrd=\S+) (.*)$`, fmt.Sprintf("${1},%s,%s ${2}", ramDiskImagePath, nmstateDiskImagePath)); err != nil {
		return err
	}

	return nil
}

func editFile(fileName string, reString string, replacement string) error {
	content, err := os.ReadFile(fileName)
	if err != nil {
		return err
	}

	re := regexp.MustCompile(reString)
	newContent := re.ReplaceAllString(string(content), replacement)

	if err := os.WriteFile(fileName, []byte(newContent), 0600); err != nil {
		return err
	}

	return nil
}
