package docker

import (
	"archive/tar"
	"fmt"
	"github.com/wagoodman/dive/dive/filetree"
	"github.com/wagoodman/dive/dive/image"
	"github.com/wagoodman/dive/utils"
	"io"
	"io/ioutil"
	"strings"
)

type ImageArchive struct {
	manifest  manifest
	config    config
	layerMap  map[string]*filetree.FileTree
}

func NewImageFromArchive(tarFile io.ReadCloser) (*ImageArchive, error) {
	img := &ImageArchive{
		layerMap:  make(map[string]*filetree.FileTree),
	}

	tarReader := tar.NewReader(tarFile)

	// store discovered json files in a map so we can read the image in one pass
	jsonFiles := make(map[string][]byte)

	var currentLayer uint
	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			fmt.Println(err)
			utils.Exit(1)
		}

		name := header.Name

		// some layer tars can be relative layer symlinks to other layer tars
		if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeReg {

			if strings.HasSuffix(name, ".tar") {
				currentLayer++
				if err != nil {
					return img, err
				}
				layerReader := tar.NewReader(tarReader)
				tree, err := processLayerTar(name, layerReader)

				if err != nil {
					return img, err
				}

				// add the layer to the image
				img.layerMap[tree.Name] = tree

			} else if strings.HasSuffix(name, ".json") {
				fileBuffer, err := ioutil.ReadAll(tarReader)
				if err != nil {
					return img, err
				}
				jsonFiles[name] = fileBuffer
			}
		}
	}

	manifestContent, exists := jsonFiles["manifest.json"]
	if !exists {
		return img, fmt.Errorf("could not find image manifest")
	}

	img.manifest = newManifest(manifestContent)

	configContent, exists := jsonFiles[img.manifest.ConfigPath]
	if !exists {
		return img, fmt.Errorf("could not find image config")
	}

	img.config = newConfig(configContent)

	return img, nil
}

func processLayerTar(name string, reader *tar.Reader) (*filetree.FileTree, error) {
	tree := filetree.NewFileTree()
	tree.Name = name

	fileInfos, err := getFileList(reader)
	if err != nil {
		return nil, err
	}

	for _, element := range fileInfos {
		tree.FileSize += uint64(element.Size)

		_, _, err := tree.AddPath(element.Path, element)
		if err != nil {
			return nil, err
		}
	}

	return tree, nil
}

func getFileList(tarReader *tar.Reader) ([]filetree.FileInfo, error) {
	var files []filetree.FileInfo

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			fmt.Println(err)
			utils.Exit(1)
		}

		name := header.Name

		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			return nil, fmt.Errorf("unexptected tar file: (XGlobalHeader): type=%v name=%s", header.Typeflag, name)
		case tar.TypeXHeader:
			return nil, fmt.Errorf("unexptected tar file (XHeader): type=%v name=%s", header.Typeflag, name)
		default:
			files = append(files, filetree.NewFileInfo(tarReader, header, name))
		}
	}
	return files, nil
}

func (img *ImageArchive) ToImage() (*image.Image, error) {
	trees := make([]*filetree.FileTree, 0)

	// build the content tree
	for _, treeName := range img.manifest.LayerTarPaths {
		tr, exists := img.layerMap[treeName]
		if exists {
			trees = append(trees, tr)
			continue
		}
		return nil, fmt.Errorf("could not find '%s' in parsed layers", treeName)
	}

	// build the layers array
	layers := make([]image.Layer, len(trees))

	// note that the resolver config stores images in reverse chronological order, so iterate backwards through layers
	// as you iterate chronologically through history (ignoring history items that have no layer contents)
	// Note: history is not required metadata in a docker image!
	tarPathIdx := 0
	histIdx := 0
	for layerIdx := len(trees) - 1; layerIdx >= 0; layerIdx-- {
		tree := trees[(len(trees)-1)-layerIdx]

		// ignore empty layers, we are only observing layers with content
		historyObj := historyEntry{
			CreatedBy: "(missing)",
		}
		for nextHistIdx := histIdx; nextHistIdx < len(img.config.History); nextHistIdx++ {
			if !img.config.History[nextHistIdx].EmptyLayer {
				histIdx = nextHistIdx
				break
			}
		}
		if histIdx < len(img.config.History) && !img.config.History[histIdx].EmptyLayer {
			historyObj = img.config.History[histIdx]
			histIdx++
		}

		historyObj.Size = tree.FileSize

		layers[layerIdx] = &layer{
			history: historyObj,
			index:   tarPathIdx,
			tree:    trees[layerIdx],
		}

		tarPathIdx++
	}

	return &image.Image{
		Trees: trees,
		Layers: layers,
	}, nil

}
