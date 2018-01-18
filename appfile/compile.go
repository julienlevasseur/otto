package appfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/go-getter"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/otto/helper/oneline"
	"github.com/hashicorp/terraform/dag"
)

const (
	// CompileVersion is the current version that we're on for
	// compilation formats. This can be used in the future to change
	// the directory structure and on-disk format of compiled appfiles.
	CompileVersion = 1

	CompileFilename        = "Appfile.compiled"
	CompileDepsFolder      = "deps"
	CompileImportsFolder   = "deps"
	CompileVersionFilename = "version"
)

// Compiled represents a "Compiled" Appfile. A compiled Appfile is one
// that has loaded all of its dependency Appfiles, completed its imports,
// verified it is valid, etc.
//
// Appfile compilation is a process that requires network activity and
// has to occur once. The idea is that after compilation, a fully compiled
// Appfile can then be loaded in the future without network connectivity.
// Additionally, since we can assume it is valid, we can load it very quickly.
type Compiled struct {
	// File is the raw Appfile
	File *File

	// Graph is the DAG that has all the dependencies. This is already
	// verified to have no cycles. Each vertex is a *CompiledGraphVertex.
	Graph *dag.AcyclicGraph
}

func (c *Compiled) Validate() error {
	var result error

	// First validate that there are no cycles in the dependency graph
	if cycles := c.Graph.Cycles(); len(cycles) > 0 {
		for _, cycle := range cycles {
			vertices := make([]string, len(cycle))
			for i, v := range cycle {
				vertices[i] = dag.VertexName(v)
			}

			result = multierror.Append(result, fmt.Errorf(
				"Dependency cycle: %s", strings.Join(vertices, ", ")))
		}
	}

	// Validate all the files
	var errLock sync.Mutex
	c.Graph.Walk(func(raw dag.Vertex) error {
		v := raw.(*CompiledGraphVertex)
		if err := v.File.Validate(); err != nil {
			errLock.Lock()
			defer errLock.Unlock()

			if s := v.File.Source; s != "" {
				err = multierror.Prefix(err, fmt.Sprintf("Dependency %s:", s))
			}

			result = multierror.Append(result, err)
		}

		return nil
	})

	return result
}

func (c *Compiled) String() string {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("Compiled Appfile: %s\n\n", c.File.Path))
	buf.WriteString("Dep Graph:\n")
	buf.WriteString(c.Graph.String())
	buf.WriteString("\n")
	return buf.String()
}

// CompiledGraphVertex is the type of the vertex within the Graph of Compiled.
type CompiledGraphVertex struct {
	// File is the raw Appfile that this represents
	File *File

	// Dir is the directory of the data root for this dependency. This
	// is only non-empty for dependencies (the root vertex does not have
	// this value).
	Dir string

	// Don't use this outside of this package.
	NameValue string
}

func (v *CompiledGraphVertex) Name() string {
	return v.NameValue
}

// CompileOpts are the options for compilation.
type CompileOpts struct {
	// Dir is the directory where all the compiled data will be stored.
	// For use of Otto with a compiled Appfile, this directory must not
	// be deleted.
	Dir string

	// Loader is called to load an Appfile in the given directory.
	// This can return the file as-is, but this point gives the caller
	// an opportunity to modify the Appfile prior to full compilation.
	//
	// The File given will already have all the imports merged.
	Loader func(f *File, dir string) (*File, error)

	// Callback is an optional way to receive notifications of events
	// during the compilation process. The CompileEvent argument should be
	// type switched to determine what it is.
	Callback func(CompileEvent)
}

// Compiler is responsible for compiling Appfiles. For each instance
// of the compiler, the directory where Appfile data is stored is cleared
// and reloaded.
//
// Multiple calls to Compile can be made with a single Appfile and the
// dependencies won't be reloaded.
type Compiler struct {
	opts          *CompileOpts
	depStorage    getter.Storage
	importCache   map[string]*File
	importLock    sync.Mutex
	importStorage getter.Storage
}

// CompileEvent is a potential event that a Callback can receive during
// Compilation.
type CompileEvent interface{}

// CompileEventDep is the event that is called when a dependency is
// being loaded.
type CompileEventDep struct {
	Source string
}

// CompileEventImport is the event that is called when an import statement
// is being loaded and merged.
type CompileEventImport struct {
	Source string
}

// LoadCompiled loads and verifies a compiled Appfile (*Compiled) from
// disk.
func LoadCompiled(dir string) (*Compiled, error) {
	// Check the version
	vsnStr, err := oneline.Read(filepath.Join(dir, CompileVersionFilename))
	if err != nil {
		return nil, err
	}
	vsn, err := strconv.ParseInt(vsnStr, 0, 0)
	if err != nil {
		return nil, err
	}

	// If the version is too new, then we can't handle it
	if vsn > CompileVersion {
		return nil, fmt.Errorf(
			"The Appfile for this enviroment was compiled with a newer version\n" +
				"of Otto. Otto can't load this environment. You can recompile this\n" +
				"environment to this version of Otto with `otto compile`.")
	}

	f, err := os.Open(filepath.Join(dir, CompileFilename))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var c Compiled
	dec := json.NewDecoder(f)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}

	return &c, nil
}

// NewCompiler initializes a compiler with the given options.
func NewCompiler(opts *CompileOpts) (*Compiler, error) {
	// Create the directory if it doesn't already exist
	if err := os.MkdirAll(opts.Dir, 0755); err != nil {
		return nil, err
	}

	// Setup our result
	c := &Compiler{opts: opts}

	// Setup our import storage and locks
	c.importCache = make(map[string]*File)
	c.importStorage = &getter.FolderStorage{
		StorageDir: filepath.Join(opts.Dir, CompileImportsFolder)}

	// Setup dep storage
	c.depStorage = &getter.FolderStorage{
		StorageDir: filepath.Join(opts.Dir, CompileDepsFolder)}
	return c, nil
}

// Compile compiles an Appfile.
//
// This may require network connectivity if there are imports or
// non-local dependencies. The repositories that dependencies point to
// will be fully loaded into the given directory, and the compiled Appfile
// will be saved there.
//
// LoadCompiled can be used to load a pre-compiled Appfile.
//
// If you have no interest in reloading a compiled Appfile, you can
// recursively delete the compilation directory after this is completed.
// Note that certain functions of Otto such as development environments
// will depend on those directories existing, however.
func (c *Compiler) Compile(f *File) (*Compiled, error) {
	// Write the version of the compilation that we'll be completing.
	if err := compileVersion(c.opts.Dir); err != nil {
		return nil, fmt.Errorf("Error writing compiled Appfile version: %s", err)
	}

	// Check if we have an ID for this or not. If we don't, then we need
	// to write the ID file. We only do this if the file has a path.
	if f.Path != "" {
		hasID, err := f.hasID()
		if err != nil {
			return nil, fmt.Errorf(
				"Error checking for Appfile UUID: %s", err)
		}

		if !hasID {
			if err := f.initID(); err != nil {
				return nil, fmt.Errorf(
					"Error writing UUID for this Appfile: %s", err)
			}
		}

		if err := f.loadID(); err != nil {
			return nil, fmt.Errorf(
				"Error loading Appfile UUID: %s", err)
		}
	}

	// Do a minimum compile to start
	compiled, err := c.MinCompile(f)
	if err != nil {
		return nil, err
	}

	// Validate the root early
	if err := compiled.File.Validate(); err != nil {
		return nil, err
	}

	// Get our root vertex
	root, err := compiled.Graph.Root()
	if err != nil {
		return nil, err
	}
	vertex := root.(*CompiledGraphVertex)

	// Build the storage we'll use for storing downloaded dependencies,
	// then use that to trigger the recursive call to download all our
	// dependencies.
	if err := c.compileDependencies(vertex, compiled.Graph); err != nil {
		return nil, err
	}

	// Validate the compiled file tree.
	if err := compiled.Validate(); err != nil {
		return nil, err
	}

	// Write the compiled Appfile data
	if err := compileWrite(c.opts.Dir, compiled); err != nil {
		return nil, err
	}

	return compiled, nil
}

// MinCompile does a minimal compilation of the given Appfile.
//
// This will load and merge any imports. This is used for a very basic
// Compiled Appfile that can be used with Otto core.
//
// This does not fetch dependencies.
func (c *Compiler) MinCompile(f *File) (*Compiled, error) {
	// Start building our compiled Appfile
	compiled := &Compiled{File: f, Graph: new(dag.AcyclicGraph)}

	// Load the imports for this single Appfile
	if err := c.compileImports(f); err != nil {
		return nil, err
	}

	// Add our root vertex for this Appfile
	vertex := &CompiledGraphVertex{File: f, NameValue: f.Application.Name}
	compiled.Graph.Add(vertex)

	return compiled, nil
}

func (c *Compiler) compileDependencies(root *CompiledGraphVertex, graph *dag.AcyclicGraph) error {
	// For easier reference below
	storage := c.depStorage

	// Make a map to keep track of the dep source to vertex mapping
	vertexMap := make(map[string]*CompiledGraphVertex)

	// Store ourselves in the map
	key, err := getter.Detect(
		".", filepath.Dir(root.File.Path),
		getter.Detectors)
	if err != nil {
		return err
	}
	vertexMap[key] = root

	// Make a queue for the other vertices we need to still get
	// dependencies for. We arbitrarily make the cap for this slice
	// 30, since that is a ton of dependencies and we don't expect the
	// average case to have more than this.
	queue := make([]*CompiledGraphVertex, 1, 30)
	queue[0] = root

	// While we still have dependencies to get, continue loading them.
	// TODO: parallelize
	for len(queue) > 0 {
		var current *CompiledGraphVertex
		current, queue = queue[len(queue)-1], queue[:len(queue)-1]

		log.Printf("[DEBUG] compiling dependencies for: %s", current.Name())
		for _, dep := range current.File.Application.Dependencies {
			key, err := getter.Detect(
				dep.Source, filepath.Dir(current.File.Path),
				getter.Detectors)
			if err != nil {
				return fmt.Errorf(
					"Error loading source: %s", err)
			}

			vertex := vertexMap[key]
			if vertex == nil {
				log.Printf("[DEBUG] loading dependency: %s", key)

				// Call the callback if we have one
				if c.opts.Callback != nil {
					c.opts.Callback(&CompileEventDep{
						Source: key,
					})
				}

				// Download the dependency
				if err := storage.Get(key, key, true); err != nil {
					return err
				}
				dir, _, err := storage.Dir(key)
				if err != nil {
					return err
				}

				// Parse the Appfile if it exists
				var f *File
				appfilePath := filepath.Join(dir, "Appfile")
				_, err = os.Stat(appfilePath)
				if err != nil && !os.IsNotExist(err) {
					return fmt.Errorf(
						"Error parsing Appfile in %s: %s", key, err)
				}
				if err == nil {
					f, err = ParseFile(appfilePath)
					if err != nil {
						return fmt.Errorf(
							"Error parsing Appfile in %s: %s", key, err)
					}

					// Realize all the imports for this file
					if err := c.compileImports(f); err != nil {
						return err
					}
				}

				// Do any additional loading if we have a loader
				if c.opts.Loader != nil {
					f, err = c.opts.Loader(f, dir)
					if err != nil {
						return fmt.Errorf(
							"Error loading Appfile in %s: %s", key, err)
					}
				}

				// Set the source
				f.Source = key

				// If it doesn't have an otto ID then we can't do anything
				hasID, err := f.hasID()
				if err != nil {
					return fmt.Errorf(
						"Error checking for ID file for Appfile in %s: %s",
						key, err)
				}
				if !hasID {
					return fmt.Errorf(
						"Dependency '%s' doesn't have an Otto ID yet!\n\n"+
							"An Otto ID is generated on the first compilation of the Appfile.\n"+
							"It is a globally unique ID that is used to track the application\n"+
							"across multiple deploys. It is required for the application to be\n"+
							"used as a dependency. To fix this, check out that application and\n"+
							"compile the Appfile with `otto compile` once. Make sure you commit\n"+
							"the .ottoid file into version control, and then try this command\n"+
							"again.",
						key)
				}

				// We merge the root infrastructure choice upwards to
				// all dependencies.
				f.Infrastructure = root.File.Infrastructure
				if root.File.Project != nil {
					if f.Project == nil {
						f.Project = new(Project)
					}
					f.Project.Infrastructure = root.File.Project.Infrastructure
				}

				// Build the vertex for this
				vertex = &CompiledGraphVertex{
					File:      f,
					Dir:       dir,
					NameValue: f.Application.Name,
				}

				// Add the vertex since it is new, store the mapping, and
				// queue it to be loaded later.
				graph.Add(vertex)
				vertexMap[key] = vertex
				queue = append(queue, vertex)
			}

			// Connect the dependencies
			graph.Connect(dag.BasicEdge(current, vertex))
		}
	}

	return nil
}

type compileImportOpts struct {
	Storage   getter.Storage
	Cache     map[string]*File
	CacheLock *sync.Mutex
}

// compileImports takes a File, loads all the imports, and merges them
// into the File.
func (c *Compiler) compileImports(root *File) error {
	// If we have no imports, short-circuit the whole thing
	if len(root.Imports) == 0 {
		return nil
	}

	// Pull these out into variables so they're easier to reference
	storage := c.importStorage
	cache := c.importCache
	cacheLock := &c.importLock

	// A graph is used to track for cycles
	var graphLock sync.Mutex
	graph := new(dag.AcyclicGraph)
	graph.Add("root")

	// Since we run the import in parallel, multiple errors can happen
	// at the same time. We use multierror and a lock to keep track of errors.
	var resultErr error
	var resultErrLock sync.Mutex

	// Forward declarations for some nested functions we use. The docs
	// for these functions are above each.
	var importSingle func(parent string, f *File) bool
	var downloadSingle func(string, *sync.WaitGroup, *sync.Mutex, []*File, int)

	// importSingle is responsible for kicking off the imports and merging
	// them for a single file. This will return true on success, false on
	// failure. On failure, it is expected that any errors are appended to
	// resultErr.
	importSingle = func(parent string, f *File) bool {
		var wg sync.WaitGroup

		// Build the list of files we'll merge later
		var mergeLock sync.Mutex
		merge := make([]*File, len(f.Imports))

		// Go through the imports and kick off the download
		for idx, i := range f.Imports {
			source, err := getter.Detect(
				i.Source, filepath.Dir(f.Path),
				getter.Detectors)
			if err != nil {
				resultErrLock.Lock()
				defer resultErrLock.Unlock()
				resultErr = multierror.Append(resultErr, fmt.Errorf(
					"Error loading import source: %s", err))
				return false
			}

			// Add this to the graph and check now if there are cycles
			graphLock.Lock()
			graph.Add(source)
			graph.Connect(dag.BasicEdge(parent, source))
			cycles := graph.Cycles()
			graphLock.Unlock()
			if len(cycles) > 0 {
				for _, cycle := range cycles {
					names := make([]string, len(cycle))
					for i, v := range cycle {
						names[i] = dag.VertexName(v)
					}

					resultErrLock.Lock()
					defer resultErrLock.Unlock()
					resultErr = multierror.Append(resultErr, fmt.Errorf(
						"Cycle found: %s", strings.Join(names, ", ")))
					return false
				}
			}

			wg.Add(1)
			go downloadSingle(source, &wg, &mergeLock, merge, idx)
		}

		// Wait for completion
		wg.Wait()

		// Go through the merge list and look for any nil entries, which
		// means that download failed. In that case, return immediately.
		// We assume any errors were put into resultErr.
		for _, importF := range merge {
			if importF == nil {
				return false
			}
		}

		for _, importF := range merge {
			// We need to copy importF here so that we don't poison
			// the cache by modifying the same pointer.
			importFCopy := *importF
			importF = &importFCopy
			source := importF.ID
			importF.ID = ""
			importF.Path = ""

			// Merge it into our file!
			if err := f.Merge(importF); err != nil {
				resultErrLock.Lock()
				defer resultErrLock.Unlock()
				resultErr = multierror.Append(resultErr, fmt.Errorf(
					"Error merging import %s: %s", source, err))
				return false
			}
		}

		return true
	}

	// downloadSingle is used to download a single import and parse the
	// Appfile. This is a separate function because it is generally run
	// in a goroutine so we can parallelize grabbing the imports.
	downloadSingle = func(source string, wg *sync.WaitGroup, l *sync.Mutex, result []*File, idx int) {
		defer wg.Done()

		// Read from the cache if we have it
		cacheLock.Lock()
		cached, ok := cache[source]
		cacheLock.Unlock()
		if ok {
			log.Printf("[DEBUG] cache hit on import: %s", source)
			l.Lock()
			defer l.Unlock()
			result[idx] = cached
			return
		}

		// Call the callback if we have one
		log.Printf("[DEBUG] loading import: %s", source)
		if c.opts.Callback != nil {
			c.opts.Callback(&CompileEventImport{
				Source: source,
			})
		}

		// Download the dependency
		if err := storage.Get(source, source, true); err != nil {
			resultErrLock.Lock()
			defer resultErrLock.Unlock()
			resultErr = multierror.Append(resultErr, fmt.Errorf(
				"Error loading import source: %s", err))
			return
		}
		dir, _, err := storage.Dir(source)
		if err != nil {
			resultErrLock.Lock()
			defer resultErrLock.Unlock()
			resultErr = multierror.Append(resultErr, fmt.Errorf(
				"Error loading import source: %s", err))
			return
		}

		// Parse the Appfile
		importF, err := ParseFile(filepath.Join(dir, "Appfile"))
		if err != nil {
			resultErrLock.Lock()
			defer resultErrLock.Unlock()
			resultErr = multierror.Append(resultErr, fmt.Errorf(
				"Error parsing Appfile in %s: %s", source, err))
			return
		}

		// We use the ID to store the source, but we clear it
		// when we actually merge.
		importF.ID = source

		// Import the imports in this
		if !importSingle(source, importF) {
			return
		}

		// Once we're done, acquire the lock and write it
		l.Lock()
		result[idx] = importF
		l.Unlock()

		// Write this into the cache.
		cacheLock.Lock()
		cache[source] = importF
		cacheLock.Unlock()
	}

	importSingle("root", root)
	return resultErr
}

func compileVersion(dir string) error {
	f, err := os.Create(filepath.Join(dir, CompileVersionFilename))
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = fmt.Fprintf(f, "%d", CompileVersion)
	return err
}

func compileWrite(dir string, compiled *Compiled) error {
	// Pretty-print the JSON data so that it can be more easily inspected
	data, err := json.MarshalIndent(compiled, "", "    ")
	if err != nil {
		return err
	}

	// Write it out
	f, err := os.Create(filepath.Join(dir, CompileFilename))
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, bytes.NewReader(data))
	return err
}
