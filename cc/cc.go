// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cc

// This file contains the module types for compiling C/C++ for Android, and converts the properties
// into the flags and filenames necessary to pass to the compiler.  The final creation of the rules
// is handled in builder.go

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/google/blueprint"
	"github.com/google/blueprint/proptools"

	"android/soong/android"
	"android/soong/cc/config"
	"android/soong/genrule"
)

func init() {
	RegisterCCBuildComponents(android.InitRegistrationContext)

	pctx.Import("android/soong/cc/config")
}

func RegisterCCBuildComponents(ctx android.RegistrationContext) {
	ctx.RegisterModuleType("cc_defaults", defaultsFactory)

	ctx.PreDepsMutators(func(ctx android.RegisterMutatorsContext) {
		ctx.BottomUp("sdk", sdkMutator).Parallel()
		ctx.BottomUp("vndk", VndkMutator).Parallel()
		ctx.BottomUp("link", LinkageMutator).Parallel()
		ctx.BottomUp("test_per_src", TestPerSrcMutator).Parallel()
		ctx.BottomUp("version_selector", versionSelectorMutator).Parallel()
		ctx.BottomUp("version", versionMutator).Parallel()
		ctx.BottomUp("begin", BeginMutator).Parallel()
		ctx.BottomUp("sysprop_cc", SyspropMutator).Parallel()
		ctx.BottomUp("vendor_snapshot", VendorSnapshotMutator).Parallel()
		ctx.BottomUp("vendor_snapshot_source", VendorSnapshotSourceMutator).Parallel()
	})

	ctx.PostDepsMutators(func(ctx android.RegisterMutatorsContext) {
		ctx.TopDown("asan_deps", sanitizerDepsMutator(asan))
		ctx.BottomUp("asan", sanitizerMutator(asan)).Parallel()

		ctx.TopDown("hwasan_deps", sanitizerDepsMutator(hwasan))
		ctx.BottomUp("hwasan", sanitizerMutator(hwasan)).Parallel()

		ctx.TopDown("fuzzer_deps", sanitizerDepsMutator(fuzzer))
		ctx.BottomUp("fuzzer", sanitizerMutator(fuzzer)).Parallel()

		// cfi mutator shouldn't run before sanitizers that return true for
		// incompatibleWithCfi()
		ctx.TopDown("cfi_deps", sanitizerDepsMutator(cfi))
		ctx.BottomUp("cfi", sanitizerMutator(cfi)).Parallel()

		ctx.TopDown("scs_deps", sanitizerDepsMutator(scs))
		ctx.BottomUp("scs", sanitizerMutator(scs)).Parallel()

		ctx.TopDown("tsan_deps", sanitizerDepsMutator(tsan))
		ctx.BottomUp("tsan", sanitizerMutator(tsan)).Parallel()

		ctx.TopDown("sanitize_runtime_deps", sanitizerRuntimeDepsMutator).Parallel()
		ctx.BottomUp("sanitize_runtime", sanitizerRuntimeMutator).Parallel()

		ctx.BottomUp("coverage", coverageMutator).Parallel()
		ctx.TopDown("vndk_deps", sabiDepsMutator)

		ctx.TopDown("lto_deps", ltoDepsMutator)
		ctx.BottomUp("lto", ltoMutator).Parallel()

		ctx.BottomUp("check_linktype", checkLinkTypeMutator).Parallel()
		ctx.TopDown("double_loadable", checkDoubleLoadableLibraries).Parallel()
	})

	android.RegisterSingletonType("kythe_extract_all", kytheExtractAllFactory)
}

type Deps struct {
	SharedLibs, LateSharedLibs                  []string
	StaticLibs, LateStaticLibs, WholeStaticLibs []string
	HeaderLibs                                  []string
	RuntimeLibs                                 []string

	// Used for data dependencies adjacent to tests
	DataLibs []string

	// Used by DepsMutator to pass system_shared_libs information to check_elf_file.py.
	SystemSharedLibs []string

	StaticUnwinderIfLegacy bool

	ReexportSharedLibHeaders, ReexportStaticLibHeaders, ReexportHeaderLibHeaders []string

	ObjFiles []string

	GeneratedSources []string
	GeneratedHeaders []string
	GeneratedDeps    []string

	ReexportGeneratedHeaders []string

	CrtBegin, CrtEnd string

	// Used for host bionic
	LinkerFlagsFile string
	DynamicLinker   string
}

type PathDeps struct {
	// Paths to .so files
	SharedLibs, EarlySharedLibs, LateSharedLibs android.Paths
	// Paths to the dependencies to use for .so files (.so.toc files)
	SharedLibsDeps, EarlySharedLibsDeps, LateSharedLibsDeps android.Paths
	// Paths to .a files
	StaticLibs, LateStaticLibs, WholeStaticLibs android.Paths

	// Transitive static library dependencies of static libraries for use in ordering.
	TranstiveStaticLibrariesForOrdering *android.DepSet

	// Paths to .o files
	Objs Objects
	// Paths to .o files in dependencies that provide them. Note that these lists
	// aren't complete since prebuilt modules don't provide the .o files.
	StaticLibObjs      Objects
	WholeStaticLibObjs Objects

	// Paths to .a files in prebuilts. Complements WholeStaticLibObjs to contain
	// the libs from all whole_static_lib dependencies.
	WholeStaticLibsFromPrebuilts android.Paths

	// Paths to generated source files
	GeneratedSources android.Paths
	GeneratedDeps    android.Paths

	Flags                      []string
	IncludeDirs                android.Paths
	SystemIncludeDirs          android.Paths
	ReexportedDirs             android.Paths
	ReexportedSystemDirs       android.Paths
	ReexportedFlags            []string
	ReexportedGeneratedHeaders android.Paths
	ReexportedDeps             android.Paths

	// Paths to crt*.o files
	CrtBegin, CrtEnd android.OptionalPath

	// Path to the file container flags to use with the linker
	LinkerFlagsFile android.OptionalPath

	// Path to the dynamic linker binary
	DynamicLinker android.OptionalPath
}

// LocalOrGlobalFlags contains flags that need to have values set globally by the build system or locally by the module
// tracked separately, in order to maintain the required ordering (most of the global flags need to go first on the
// command line so they can be overridden by the local module flags).
type LocalOrGlobalFlags struct {
	CommonFlags     []string // Flags that apply to C, C++, and assembly source files
	AsFlags         []string // Flags that apply to assembly source files
	YasmFlags       []string // Flags that apply to yasm assembly source files
	CFlags          []string // Flags that apply to C and C++ source files
	ToolingCFlags   []string // Flags that apply to C and C++ source files parsed by clang LibTooling tools
	ConlyFlags      []string // Flags that apply to C source files
	CppFlags        []string // Flags that apply to C++ source files
	ToolingCppFlags []string // Flags that apply to C++ source files parsed by clang LibTooling tools
	LdFlags         []string // Flags that apply to linker command lines
}

type Flags struct {
	Local  LocalOrGlobalFlags
	Global LocalOrGlobalFlags

	aidlFlags     []string // Flags that apply to aidl source files
	rsFlags       []string // Flags that apply to renderscript source files
	libFlags      []string // Flags to add libraries early to the link order
	extraLibFlags []string // Flags to add libraries late in the link order after LdFlags
	TidyFlags     []string // Flags that apply to clang-tidy
	SAbiFlags     []string // Flags that apply to header-abi-dumper

	// Global include flags that apply to C, C++, and assembly source files
	// These must be after any module include flags, which will be in CommonFlags.
	SystemIncludeFlags []string

	Toolchain    config.Toolchain
	Tidy         bool
	GcovCoverage bool
	SAbiDump     bool
	EmitXrefs    bool // If true, generate Ninja rules to generate emitXrefs input files for Kythe

	RequiredInstructionSet string
	DynamicLinker          string

	CFlagsDeps  android.Paths // Files depended on by compiler flags
	LdFlagsDeps android.Paths // Files depended on by linker flags

	AssemblerWithCpp bool
	GroupStaticLibs  bool

	proto            android.ProtoFlags
	protoC           bool // Whether to use C instead of C++
	protoOptionsFile bool // Whether to look for a .options file next to the .proto

	Yacc *YaccProperties
	Lex  *LexProperties
}

// Properties used to compile all C or C++ modules
type BaseProperties struct {
	// Deprecated. true is the default, false is invalid.
	Clang *bool `android:"arch_variant"`

	// Minimum sdk version supported when compiling against the ndk. Setting this property causes
	// two variants to be built, one for the platform and one for apps.
	Sdk_version *string

	// Minimum sdk version that the artifact should support when it runs as part of mainline modules(APEX).
	Min_sdk_version *string

	// If true, always create an sdk variant and don't create a platform variant.
	Sdk_variant_only *bool

	AndroidMkSharedLibs       []string `blueprint:"mutated"`
	AndroidMkStaticLibs       []string `blueprint:"mutated"`
	AndroidMkRuntimeLibs      []string `blueprint:"mutated"`
	AndroidMkWholeStaticLibs  []string `blueprint:"mutated"`
	AndroidMkHeaderLibs       []string `blueprint:"mutated"`
	HideFromMake              bool     `blueprint:"mutated"`
	PreventInstall            bool     `blueprint:"mutated"`
	ApexesProvidingSharedLibs []string `blueprint:"mutated"`

	// Set by DepsMutator.
	AndroidMkSystemSharedLibs []string `blueprint:"mutated"`

	ImageVariationPrefix string `blueprint:"mutated"`
	VndkVersion          string `blueprint:"mutated"`
	SubName              string `blueprint:"mutated"`

	// *.logtags files, to combine together in order to generate the /system/etc/event-log-tags
	// file
	Logtags []string

	// Make this module available when building for ramdisk.
	// On device without a dedicated recovery partition, the module is only
	// available after switching root into
	// /first_stage_ramdisk. To expose the module before switching root, install
	// the recovery variant instead.
	Ramdisk_available *bool

	// Make this module available when building for vendor ramdisk.
	// On device without a dedicated recovery partition, the module is only
	// available after switching root into
	// /first_stage_ramdisk. To expose the module before switching root, install
	// the recovery variant instead.
	Vendor_ramdisk_available *bool

	// Make this module available when building for recovery
	Recovery_available *bool

	// Set by imageMutator
	CoreVariantNeeded          bool     `blueprint:"mutated"`
	RamdiskVariantNeeded       bool     `blueprint:"mutated"`
	VendorRamdiskVariantNeeded bool     `blueprint:"mutated"`
	RecoveryVariantNeeded      bool     `blueprint:"mutated"`
	ExtraVariants              []string `blueprint:"mutated"`

	// Allows this module to use non-APEX version of libraries. Useful
	// for building binaries that are started before APEXes are activated.
	Bootstrap *bool

	// Even if DeviceConfig().VndkUseCoreVariant() is set, this module must use vendor variant.
	// see soong/cc/config/vndk.go
	MustUseVendorVariant bool `blueprint:"mutated"`

	// Used by vendor snapshot to record dependencies from snapshot modules.
	SnapshotSharedLibs  []string `blueprint:"mutated"`
	SnapshotRuntimeLibs []string `blueprint:"mutated"`

	Installable *bool

	// Set by factories of module types that can only be referenced from variants compiled against
	// the SDK.
	AlwaysSdk bool `blueprint:"mutated"`

	// Variant is an SDK variant created by sdkMutator
	IsSdkVariant bool `blueprint:"mutated"`
	// Set when both SDK and platform variants are exported to Make to trigger renaming the SDK
	// variant to have a ".sdk" suffix.
	SdkAndPlatformVariantVisibleToMake bool `blueprint:"mutated"`

	// Normally Soong uses the directory structure to decide which modules
	// should be included (framework) or excluded (non-framework) from the
	// vendor snapshot, but this property allows a partner to exclude a
	// module normally thought of as a framework module from the vendor
	// snapshot.
	Exclude_from_vendor_snapshot *bool
}

type VendorProperties struct {
	// whether this module should be allowed to be directly depended by other
	// modules with `vendor: true`, `proprietary: true`, or `vendor_available:true`.
	// In addition, this module should be allowed to be directly depended by
	// product modules with `product_specific: true`.
	// If set to true, three variants will be built separately, one like
	// normal, another limited to the set of libraries and headers
	// that are exposed to /vendor modules, and the other to /product modules.
	//
	// The vendor and product variants may be used with a different (newer) /system,
	// so it shouldn't have any unversioned runtime dependencies, or
	// make assumptions about the system that may not be true in the
	// future.
	//
	// If set to false, this module becomes inaccessible from /vendor or /product
	// modules.
	//
	// Default value is true when vndk: {enabled: true} or vendor: true.
	//
	// Nothing happens if BOARD_VNDK_VERSION isn't set in the BoardConfig.mk
	// If PRODUCT_PRODUCT_VNDK_VERSION isn't set, product variant will not be used.
	Vendor_available *bool

	// whether this module is capable of being loaded with other instance
	// (possibly an older version) of the same module in the same process.
	// Currently, a shared library that is a member of VNDK (vndk: {enabled: true})
	// can be double loaded in a vendor process if the library is also a
	// (direct and indirect) dependency of an LLNDK library. Such libraries must be
	// explicitly marked as `double_loadable: true` by the owner, or the dependency
	// from the LLNDK lib should be cut if the lib is not designed to be double loaded.
	Double_loadable *bool
}

type ModuleContextIntf interface {
	static() bool
	staticBinary() bool
	header() bool
	binary() bool
	object() bool
	toolchain() config.Toolchain
	canUseSdk() bool
	useSdk() bool
	sdkVersion() string
	useVndk() bool
	isNdk() bool
	isLlndk(config android.Config) bool
	isLlndkPublic(config android.Config) bool
	isVndkPrivate(config android.Config) bool
	isVndk() bool
	isVndkSp() bool
	isVndkExt() bool
	inProduct() bool
	inVendor() bool
	inRamdisk() bool
	inVendorRamdisk() bool
	inRecovery() bool
	shouldCreateSourceAbiDump() bool
	selectedStl() string
	baseModuleName() string
	getVndkExtendsModuleName() string
	isPgoCompile() bool
	isNDKStubLibrary() bool
	useClangLld(actx ModuleContext) bool
	isForPlatform() bool
	apexVariationName() string
	apexSdkVersion() android.ApiLevel
	bootstrap() bool
	mustUseVendorVariant() bool
	nativeCoverage() bool
	directlyInAnyApex() bool
}

type ModuleContext interface {
	android.ModuleContext
	ModuleContextIntf
}

type BaseModuleContext interface {
	android.BaseModuleContext
	ModuleContextIntf
}

type DepsContext interface {
	android.BottomUpMutatorContext
	ModuleContextIntf
}

type feature interface {
	begin(ctx BaseModuleContext)
	deps(ctx DepsContext, deps Deps) Deps
	flags(ctx ModuleContext, flags Flags) Flags
	props() []interface{}
}

type compiler interface {
	compilerInit(ctx BaseModuleContext)
	compilerDeps(ctx DepsContext, deps Deps) Deps
	compilerFlags(ctx ModuleContext, flags Flags, deps PathDeps) Flags
	compilerProps() []interface{}

	appendCflags([]string)
	appendAsflags([]string)
	compile(ctx ModuleContext, flags Flags, deps PathDeps) Objects
}

type linker interface {
	linkerInit(ctx BaseModuleContext)
	linkerDeps(ctx DepsContext, deps Deps) Deps
	linkerFlags(ctx ModuleContext, flags Flags) Flags
	linkerProps() []interface{}
	useClangLld(actx ModuleContext) bool

	link(ctx ModuleContext, flags Flags, deps PathDeps, objs Objects) android.Path
	appendLdflags([]string)
	unstrippedOutputFilePath() android.Path

	nativeCoverage() bool
	coverageOutputFilePath() android.OptionalPath

	// Get the deps that have been explicitly specified in the properties.
	// Only updates the
	linkerSpecifiedDeps(specifiedDeps specifiedDeps) specifiedDeps
}

type specifiedDeps struct {
	sharedLibs       []string
	systemSharedLibs []string // Note nil and [] are semantically distinct.
}

type installer interface {
	installerProps() []interface{}
	install(ctx ModuleContext, path android.Path)
	everInstallable() bool
	inData() bool
	inSanitizerDir() bool
	hostToolPath() android.OptionalPath
	relativeInstallPath() string
	makeUninstallable(mod *Module)
}

type xref interface {
	XrefCcFiles() android.Paths
}

type libraryDependencyKind int

const (
	headerLibraryDependency = iota
	sharedLibraryDependency
	staticLibraryDependency
)

func (k libraryDependencyKind) String() string {
	switch k {
	case headerLibraryDependency:
		return "headerLibraryDependency"
	case sharedLibraryDependency:
		return "sharedLibraryDependency"
	case staticLibraryDependency:
		return "staticLibraryDependency"
	default:
		panic(fmt.Errorf("unknown libraryDependencyKind %d", k))
	}
}

type libraryDependencyOrder int

const (
	earlyLibraryDependency  = -1
	normalLibraryDependency = 0
	lateLibraryDependency   = 1
)

func (o libraryDependencyOrder) String() string {
	switch o {
	case earlyLibraryDependency:
		return "earlyLibraryDependency"
	case normalLibraryDependency:
		return "normalLibraryDependency"
	case lateLibraryDependency:
		return "lateLibraryDependency"
	default:
		panic(fmt.Errorf("unknown libraryDependencyOrder %d", o))
	}
}

// libraryDependencyTag is used to tag dependencies on libraries.  Unlike many dependency
// tags that have a set of predefined tag objects that are reused for each dependency, a
// libraryDependencyTag is designed to contain extra metadata and is constructed as needed.
// That means that comparing a libraryDependencyTag for equality will only be equal if all
// of the metadata is equal.  Most usages will want to type assert to libraryDependencyTag and
// then check individual metadata fields instead.
type libraryDependencyTag struct {
	blueprint.BaseDependencyTag

	// These are exported so that fmt.Printf("%#v") can call their String methods.
	Kind  libraryDependencyKind
	Order libraryDependencyOrder

	wholeStatic bool

	reexportFlags       bool
	explicitlyVersioned bool
	dataLib             bool
	ndk                 bool

	staticUnwinder bool

	makeSuffix string
}

// header returns true if the libraryDependencyTag is tagging a header lib dependency.
func (d libraryDependencyTag) header() bool {
	return d.Kind == headerLibraryDependency
}

// shared returns true if the libraryDependencyTag is tagging a shared lib dependency.
func (d libraryDependencyTag) shared() bool {
	return d.Kind == sharedLibraryDependency
}

// shared returns true if the libraryDependencyTag is tagging a static lib dependency.
func (d libraryDependencyTag) static() bool {
	return d.Kind == staticLibraryDependency
}

// dependencyTag is used for tagging miscellanous dependency types that don't fit into
// libraryDependencyTag.  Each tag object is created globally and reused for multiple
// dependencies (although since the object contains no references, assigning a tag to a
// variable and modifying it will not modify the original).  Users can compare the tag
// returned by ctx.OtherModuleDependencyTag against the global original
type dependencyTag struct {
	blueprint.BaseDependencyTag
	name string
}

var (
	genSourceDepTag       = dependencyTag{name: "gen source"}
	genHeaderDepTag       = dependencyTag{name: "gen header"}
	genHeaderExportDepTag = dependencyTag{name: "gen header export"}
	objDepTag             = dependencyTag{name: "obj"}
	linkerFlagsDepTag     = dependencyTag{name: "linker flags file"}
	dynamicLinkerDepTag   = dependencyTag{name: "dynamic linker"}
	reuseObjTag           = dependencyTag{name: "reuse objects"}
	staticVariantTag      = dependencyTag{name: "static variant"}
	vndkExtDepTag         = dependencyTag{name: "vndk extends"}
	dataLibDepTag         = dependencyTag{name: "data lib"}
	runtimeDepTag         = dependencyTag{name: "runtime lib"}
	testPerSrcDepTag      = dependencyTag{name: "test_per_src"}
	stubImplDepTag        = dependencyTag{name: "stub_impl"}
)

type copyDirectlyInAnyApexDependencyTag dependencyTag

func (copyDirectlyInAnyApexDependencyTag) CopyDirectlyInAnyApex() {}

var _ android.CopyDirectlyInAnyApexTag = copyDirectlyInAnyApexDependencyTag{}

func IsSharedDepTag(depTag blueprint.DependencyTag) bool {
	ccLibDepTag, ok := depTag.(libraryDependencyTag)
	return ok && ccLibDepTag.shared()
}

func IsStaticDepTag(depTag blueprint.DependencyTag) bool {
	ccLibDepTag, ok := depTag.(libraryDependencyTag)
	return ok && ccLibDepTag.static()
}

func IsRuntimeDepTag(depTag blueprint.DependencyTag) bool {
	ccDepTag, ok := depTag.(dependencyTag)
	return ok && ccDepTag == runtimeDepTag
}

func IsTestPerSrcDepTag(depTag blueprint.DependencyTag) bool {
	ccDepTag, ok := depTag.(dependencyTag)
	return ok && ccDepTag == testPerSrcDepTag
}

// Module contains the properties and members used by all C/C++ module types, and implements
// the blueprint.Module interface.  It delegates to compiler, linker, and installer interfaces
// to construct the output file.  Behavior can be customized with a Customizer interface
type Module struct {
	android.ModuleBase
	android.DefaultableModuleBase
	android.ApexModuleBase
	android.SdkBase

	Properties       BaseProperties
	VendorProperties VendorProperties

	// initialize before calling Init
	hod      android.HostOrDeviceSupported
	multilib android.Multilib

	// Allowable SdkMemberTypes of this module type.
	sdkMemberTypes []android.SdkMemberType

	// delegates, initialize before calling Init
	features  []feature
	compiler  compiler
	linker    linker
	installer installer
	stl       *stl
	sanitize  *sanitize
	coverage  *coverage
	sabi      *sabi
	vndkdep   *vndkdep
	lto       *lto
	pgo       *pgo

	library libraryInterface

	outputFile android.OptionalPath

	cachedToolchain config.Toolchain

	subAndroidMkOnce map[subAndroidMkProvider]bool

	// Flags used to compile this module
	flags Flags

	// only non-nil when this is a shared library that reuses the objects of a static library
	staticAnalogue *StaticLibraryInfo

	makeLinkType string
	// Kythe (source file indexer) paths for this compilation module
	kytheFiles android.Paths

	// For apex variants, this is set as apex.min_sdk_version
	apexSdkVersion android.ApiLevel

	hideApexVariantFromMake bool
}

func (c *Module) Toc() android.OptionalPath {
	if c.linker != nil {
		if library, ok := c.linker.(libraryInterface); ok {
			return library.toc()
		}
	}
	panic(fmt.Errorf("Toc() called on non-library module: %q", c.BaseModuleName()))
}

func (c *Module) ApiLevel() string {
	if c.linker != nil {
		if stub, ok := c.linker.(*stubDecorator); ok {
			return stub.apiLevel.String()
		}
	}
	panic(fmt.Errorf("ApiLevel() called on non-stub library module: %q", c.BaseModuleName()))
}

func (c *Module) Static() bool {
	if c.linker != nil {
		if library, ok := c.linker.(libraryInterface); ok {
			return library.static()
		}
	}
	panic(fmt.Errorf("Static() called on non-library module: %q", c.BaseModuleName()))
}

func (c *Module) Shared() bool {
	if c.linker != nil {
		if library, ok := c.linker.(libraryInterface); ok {
			return library.shared()
		}
	}
	panic(fmt.Errorf("Shared() called on non-library module: %q", c.BaseModuleName()))
}

func (c *Module) SelectedStl() string {
	if c.stl != nil {
		return c.stl.Properties.SelectedStl
	}
	return ""
}

func (c *Module) ToolchainLibrary() bool {
	if _, ok := c.linker.(*toolchainLibraryDecorator); ok {
		return true
	}
	return false
}

func (c *Module) NdkPrebuiltStl() bool {
	if _, ok := c.linker.(*ndkPrebuiltStlLinker); ok {
		return true
	}
	return false
}

func (c *Module) StubDecorator() bool {
	if _, ok := c.linker.(*stubDecorator); ok {
		return true
	}
	return false
}

func (c *Module) SdkVersion() string {
	return String(c.Properties.Sdk_version)
}

func (c *Module) MinSdkVersion() string {
	return String(c.Properties.Min_sdk_version)
}

func (c *Module) SplitPerApiLevel() bool {
	if !c.canUseSdk() {
		return false
	}
	if linker, ok := c.linker.(*objectLinker); ok {
		return linker.isCrt()
	}
	return false
}

func (c *Module) AlwaysSdk() bool {
	return c.Properties.AlwaysSdk || Bool(c.Properties.Sdk_variant_only)
}

func (c *Module) CcLibrary() bool {
	if c.linker != nil {
		if _, ok := c.linker.(*libraryDecorator); ok {
			return true
		}
		if _, ok := c.linker.(*prebuiltLibraryLinker); ok {
			return true
		}
	}
	return false
}

func (c *Module) CcLibraryInterface() bool {
	if _, ok := c.linker.(libraryInterface); ok {
		return true
	}
	return false
}

func (c *Module) NonCcVariants() bool {
	return false
}

func (c *Module) SetStatic() {
	if c.linker != nil {
		if library, ok := c.linker.(libraryInterface); ok {
			library.setStatic()
			return
		}
	}
	panic(fmt.Errorf("SetStatic called on non-library module: %q", c.BaseModuleName()))
}

func (c *Module) SetShared() {
	if c.linker != nil {
		if library, ok := c.linker.(libraryInterface); ok {
			library.setShared()
			return
		}
	}
	panic(fmt.Errorf("SetShared called on non-library module: %q", c.BaseModuleName()))
}

func (c *Module) BuildStaticVariant() bool {
	if c.linker != nil {
		if library, ok := c.linker.(libraryInterface); ok {
			return library.buildStatic()
		}
	}
	panic(fmt.Errorf("BuildStaticVariant called on non-library module: %q", c.BaseModuleName()))
}

func (c *Module) BuildSharedVariant() bool {
	if c.linker != nil {
		if library, ok := c.linker.(libraryInterface); ok {
			return library.buildShared()
		}
	}
	panic(fmt.Errorf("BuildSharedVariant called on non-library module: %q", c.BaseModuleName()))
}

func (c *Module) Module() android.Module {
	return c
}

func (c *Module) OutputFile() android.OptionalPath {
	return c.outputFile
}

func (c *Module) CoverageFiles() android.Paths {
	if c.linker != nil {
		if library, ok := c.linker.(libraryInterface); ok {
			return library.objs().coverageFiles
		}
	}
	panic(fmt.Errorf("CoverageFiles called on non-library module: %q", c.BaseModuleName()))
}

var _ LinkableInterface = (*Module)(nil)

func (c *Module) UnstrippedOutputFile() android.Path {
	if c.linker != nil {
		return c.linker.unstrippedOutputFilePath()
	}
	return nil
}

func (c *Module) CoverageOutputFile() android.OptionalPath {
	if c.linker != nil {
		return c.linker.coverageOutputFilePath()
	}
	return android.OptionalPath{}
}

func (c *Module) RelativeInstallPath() string {
	if c.installer != nil {
		return c.installer.relativeInstallPath()
	}
	return ""
}

func (c *Module) VndkVersion() string {
	return c.Properties.VndkVersion
}

func (c *Module) Init() android.Module {
	c.AddProperties(&c.Properties, &c.VendorProperties)
	if c.compiler != nil {
		c.AddProperties(c.compiler.compilerProps()...)
	}
	if c.linker != nil {
		c.AddProperties(c.linker.linkerProps()...)
	}
	if c.installer != nil {
		c.AddProperties(c.installer.installerProps()...)
	}
	if c.stl != nil {
		c.AddProperties(c.stl.props()...)
	}
	if c.sanitize != nil {
		c.AddProperties(c.sanitize.props()...)
	}
	if c.coverage != nil {
		c.AddProperties(c.coverage.props()...)
	}
	if c.sabi != nil {
		c.AddProperties(c.sabi.props()...)
	}
	if c.vndkdep != nil {
		c.AddProperties(c.vndkdep.props()...)
	}
	if c.lto != nil {
		c.AddProperties(c.lto.props()...)
	}
	if c.pgo != nil {
		c.AddProperties(c.pgo.props()...)
	}
	for _, feature := range c.features {
		c.AddProperties(feature.props()...)
	}

	c.Prefer32(func(ctx android.BaseModuleContext, base *android.ModuleBase, os android.OsType) bool {
		// Windows builds always prefer 32-bit
		return os == android.Windows
	})
	android.InitAndroidArchModule(c, c.hod, c.multilib)
	android.InitApexModule(c)
	android.InitSdkAwareModule(c)
	android.InitDefaultableModule(c)

	return c
}

// Returns true for dependency roots (binaries)
// TODO(ccross): also handle dlopenable libraries
func (c *Module) isDependencyRoot() bool {
	if root, ok := c.linker.(interface {
		isDependencyRoot() bool
	}); ok {
		return root.isDependencyRoot()
	}
	return false
}

// Returns true if the module is using VNDK libraries instead of the libraries in /system/lib or /system/lib64.
// "product" and "vendor" variant modules return true for this function.
// When BOARD_VNDK_VERSION is set, vendor variants of "vendor_available: true", "vendor: true",
// "soc_specific: true" and more vendor installed modules are included here.
// When PRODUCT_PRODUCT_VNDK_VERSION is set, product variants of "vendor_available: true" or
// "product_specific: true" modules are included here.
func (c *Module) UseVndk() bool {
	return c.Properties.VndkVersion != ""
}

func (c *Module) canUseSdk() bool {
	return c.Os() == android.Android && !c.UseVndk() && !c.InRamdisk() && !c.InRecovery() && !c.InVendorRamdisk()
}

func (c *Module) UseSdk() bool {
	if c.canUseSdk() {
		return String(c.Properties.Sdk_version) != ""
	}
	return false
}

func (c *Module) isCoverageVariant() bool {
	return c.coverage.Properties.IsCoverageVariant
}

func (c *Module) IsNdk() bool {
	return inList(c.BaseModuleName(), ndkKnownLibs)
}

func (c *Module) isLlndk(config android.Config) bool {
	// Returns true for both LLNDK (public) and LLNDK-private libs.
	return isLlndkLibrary(c.BaseModuleName(), config)
}

func (c *Module) isLlndkPublic(config android.Config) bool {
	// Returns true only for LLNDK (public) libs.
	name := c.BaseModuleName()
	return isLlndkLibrary(name, config) && !isVndkPrivateLibrary(name, config)
}

func (c *Module) isVndkPrivate(config android.Config) bool {
	// Returns true for LLNDK-private, VNDK-SP-private, and VNDK-core-private.
	return isVndkPrivateLibrary(c.BaseModuleName(), config)
}

func (c *Module) IsVndk() bool {
	if vndkdep := c.vndkdep; vndkdep != nil {
		return vndkdep.isVndk()
	}
	return false
}

func (c *Module) isPgoCompile() bool {
	if pgo := c.pgo; pgo != nil {
		return pgo.Properties.PgoCompile
	}
	return false
}

func (c *Module) isNDKStubLibrary() bool {
	if _, ok := c.compiler.(*stubDecorator); ok {
		return true
	}
	return false
}

func (c *Module) isVndkSp() bool {
	if vndkdep := c.vndkdep; vndkdep != nil {
		return vndkdep.isVndkSp()
	}
	return false
}

func (c *Module) isVndkExt() bool {
	if vndkdep := c.vndkdep; vndkdep != nil {
		return vndkdep.isVndkExt()
	}
	return false
}

func (c *Module) MustUseVendorVariant() bool {
	return c.isVndkSp() || c.Properties.MustUseVendorVariant
}

func (c *Module) getVndkExtendsModuleName() string {
	if vndkdep := c.vndkdep; vndkdep != nil {
		return vndkdep.getVndkExtendsModuleName()
	}
	return ""
}

func (c *Module) IsStubs() bool {
	if lib := c.library; lib != nil {
		return lib.buildStubs()
	}
	return false
}

func (c *Module) HasStubsVariants() bool {
	if lib := c.library; lib != nil {
		return lib.hasStubsVariants()
	}
	return false
}

// If this is a stubs library, ImplementationModuleName returns the name of the module that contains
// the implementation.  If it is an implementation library it returns its own name.
func (c *Module) ImplementationModuleName(ctx android.BaseModuleContext) string {
	name := ctx.OtherModuleName(c)
	if versioned, ok := c.linker.(versionedInterface); ok {
		name = versioned.implementationModuleName(name)
	}
	return name
}

func (c *Module) bootstrap() bool {
	return Bool(c.Properties.Bootstrap)
}

func (c *Module) nativeCoverage() bool {
	// Bug: http://b/137883967 - native-bridge modules do not currently work with coverage
	if c.Target().NativeBridge == android.NativeBridgeEnabled {
		return false
	}
	return c.linker != nil && c.linker.nativeCoverage()
}

func (c *Module) isSnapshotPrebuilt() bool {
	if p, ok := c.linker.(interface{ isSnapshotPrebuilt() bool }); ok {
		return p.isSnapshotPrebuilt()
	}
	return false
}

func (c *Module) ExcludeFromVendorSnapshot() bool {
	return Bool(c.Properties.Exclude_from_vendor_snapshot)
}

func isBionic(name string) bool {
	switch name {
	case "libc", "libm", "libdl", "libdl_android", "linker":
		return true
	}
	return false
}

func InstallToBootstrap(name string, config android.Config) bool {
	if name == "libclang_rt.hwasan-aarch64-android" {
		return true
	}
	return isBionic(name)
}

func (c *Module) XrefCcFiles() android.Paths {
	return c.kytheFiles
}

type baseModuleContext struct {
	android.BaseModuleContext
	moduleContextImpl
}

type depsContext struct {
	android.BottomUpMutatorContext
	moduleContextImpl
}

type moduleContext struct {
	android.ModuleContext
	moduleContextImpl
}

type moduleContextImpl struct {
	mod *Module
	ctx BaseModuleContext
}

func (ctx *moduleContextImpl) toolchain() config.Toolchain {
	return ctx.mod.toolchain(ctx.ctx)
}

func (ctx *moduleContextImpl) static() bool {
	return ctx.mod.static()
}

func (ctx *moduleContextImpl) staticBinary() bool {
	return ctx.mod.staticBinary()
}

func (ctx *moduleContextImpl) header() bool {
	return ctx.mod.header()
}

func (ctx *moduleContextImpl) binary() bool {
	return ctx.mod.binary()
}

func (ctx *moduleContextImpl) object() bool {
	return ctx.mod.object()
}

func (ctx *moduleContextImpl) canUseSdk() bool {
	return ctx.mod.canUseSdk()
}

func (ctx *moduleContextImpl) useSdk() bool {
	return ctx.mod.UseSdk()
}

func (ctx *moduleContextImpl) sdkVersion() string {
	if ctx.ctx.Device() {
		if ctx.useVndk() {
			vndkVer := ctx.mod.VndkVersion()
			if inList(vndkVer, ctx.ctx.Config().PlatformVersionActiveCodenames()) {
				return "current"
			}
			return vndkVer
		}
		return String(ctx.mod.Properties.Sdk_version)
	}
	return ""
}

func (ctx *moduleContextImpl) useVndk() bool {
	return ctx.mod.UseVndk()
}

func (ctx *moduleContextImpl) isNdk() bool {
	return ctx.mod.IsNdk()
}

func (ctx *moduleContextImpl) isLlndk(config android.Config) bool {
	return ctx.mod.isLlndk(config)
}

func (ctx *moduleContextImpl) isLlndkPublic(config android.Config) bool {
	return ctx.mod.isLlndkPublic(config)
}

func (ctx *moduleContextImpl) isVndkPrivate(config android.Config) bool {
	return ctx.mod.isVndkPrivate(config)
}

func (ctx *moduleContextImpl) isVndk() bool {
	return ctx.mod.IsVndk()
}

func (ctx *moduleContextImpl) isPgoCompile() bool {
	return ctx.mod.isPgoCompile()
}

func (ctx *moduleContextImpl) isNDKStubLibrary() bool {
	return ctx.mod.isNDKStubLibrary()
}

func (ctx *moduleContextImpl) isVndkSp() bool {
	return ctx.mod.isVndkSp()
}

func (ctx *moduleContextImpl) isVndkExt() bool {
	return ctx.mod.isVndkExt()
}

func (ctx *moduleContextImpl) mustUseVendorVariant() bool {
	return ctx.mod.MustUseVendorVariant()
}

// Check whether ABI dumps should be created for this module.
func (ctx *moduleContextImpl) shouldCreateSourceAbiDump() bool {
	if ctx.ctx.Config().IsEnvTrue("SKIP_ABI_CHECKS") {
		return false
	}

	// Coverage builds have extra symbols.
	if ctx.mod.isCoverageVariant() {
		return false
	}

	if ctx.ctx.Fuchsia() {
		return false
	}

	if sanitize := ctx.mod.sanitize; sanitize != nil {
		if !sanitize.isVariantOnProductionDevice() {
			return false
		}
	}
	if !ctx.ctx.Device() {
		// Host modules do not need ABI dumps.
		return false
	}
	if ctx.isNDKStubLibrary() {
		// Stubs do not need ABI dumps.
		return false
	}
	if lib := ctx.mod.library; lib != nil && lib.buildStubs() {
		// Stubs do not need ABI dumps.
		return false
	}

	return true
}

func (ctx *moduleContextImpl) selectedStl() string {
	if stl := ctx.mod.stl; stl != nil {
		return stl.Properties.SelectedStl
	}
	return ""
}

func (ctx *moduleContextImpl) useClangLld(actx ModuleContext) bool {
	return ctx.mod.linker.useClangLld(actx)
}

func (ctx *moduleContextImpl) baseModuleName() string {
	return ctx.mod.ModuleBase.BaseModuleName()
}

func (ctx *moduleContextImpl) getVndkExtendsModuleName() string {
	return ctx.mod.getVndkExtendsModuleName()
}

func (ctx *moduleContextImpl) isForPlatform() bool {
	return ctx.ctx.Provider(android.ApexInfoProvider).(android.ApexInfo).IsForPlatform()
}

func (ctx *moduleContextImpl) apexVariationName() string {
	return ctx.ctx.Provider(android.ApexInfoProvider).(android.ApexInfo).ApexVariationName
}

func (ctx *moduleContextImpl) apexSdkVersion() android.ApiLevel {
	return ctx.mod.apexSdkVersion
}

func (ctx *moduleContextImpl) bootstrap() bool {
	return ctx.mod.bootstrap()
}

func (ctx *moduleContextImpl) nativeCoverage() bool {
	return ctx.mod.nativeCoverage()
}

func (ctx *moduleContextImpl) directlyInAnyApex() bool {
	return ctx.mod.DirectlyInAnyApex()
}

func newBaseModule(hod android.HostOrDeviceSupported, multilib android.Multilib) *Module {
	return &Module{
		hod:      hod,
		multilib: multilib,
	}
}

func newModule(hod android.HostOrDeviceSupported, multilib android.Multilib) *Module {
	module := newBaseModule(hod, multilib)
	module.features = []feature{
		&tidyFeature{},
	}
	module.stl = &stl{}
	module.sanitize = &sanitize{}
	module.coverage = &coverage{}
	module.sabi = &sabi{}
	module.vndkdep = &vndkdep{}
	module.lto = &lto{}
	module.pgo = &pgo{}
	return module
}

func (c *Module) Prebuilt() *android.Prebuilt {
	if p, ok := c.linker.(prebuiltLinkerInterface); ok {
		return p.prebuilt()
	}
	return nil
}

func (c *Module) Name() string {
	name := c.ModuleBase.Name()
	if p, ok := c.linker.(interface {
		Name(string) string
	}); ok {
		name = p.Name(name)
	}
	return name
}

func (c *Module) Symlinks() []string {
	if p, ok := c.installer.(interface {
		symlinkList() []string
	}); ok {
		return p.symlinkList()
	}
	return nil
}

func (c *Module) IsTestPerSrcAllTestsVariation() bool {
	test, ok := c.linker.(testPerSrc)
	return ok && test.isAllTestsVariation()
}

func (c *Module) DataPaths() []android.DataPath {
	if p, ok := c.installer.(interface {
		dataPaths() []android.DataPath
	}); ok {
		return p.dataPaths()
	}
	return nil
}

func (c *Module) getNameSuffixWithVndkVersion(ctx android.ModuleContext) string {
	// Returns the name suffix for product and vendor variants. If the VNDK version is not
	// "current", it will append the VNDK version to the name suffix.
	var vndkVersion string
	var nameSuffix string
	if c.inProduct() {
		vndkVersion = ctx.DeviceConfig().ProductVndkVersion()
		nameSuffix = productSuffix
	} else {
		vndkVersion = ctx.DeviceConfig().VndkVersion()
		nameSuffix = vendorSuffix
	}
	if vndkVersion == "current" {
		vndkVersion = ctx.DeviceConfig().PlatformVndkVersion()
	}
	if c.Properties.VndkVersion != vndkVersion {
		// add version suffix only if the module is using different vndk version than the
		// version in product or vendor partition.
		nameSuffix += "." + c.Properties.VndkVersion
	}
	return nameSuffix
}

func (c *Module) GenerateAndroidBuildActions(actx android.ModuleContext) {
	// Handle the case of a test module split by `test_per_src` mutator.
	//
	// The `test_per_src` mutator adds an extra variation named "", depending on all the other
	// `test_per_src` variations of the test module. Set `outputFile` to an empty path for this
	// module and return early, as this module does not produce an output file per se.
	if c.IsTestPerSrcAllTestsVariation() {
		c.outputFile = android.OptionalPath{}
		return
	}

	apexInfo := actx.Provider(android.ApexInfoProvider).(android.ApexInfo)
	if !apexInfo.IsForPlatform() {
		c.hideApexVariantFromMake = true
	}

	c.makeLinkType = c.getMakeLinkType(actx)

	c.Properties.SubName = ""

	if c.Target().NativeBridge == android.NativeBridgeEnabled {
		c.Properties.SubName += nativeBridgeSuffix
	}

	_, llndk := c.linker.(*llndkStubDecorator)
	_, llndkHeader := c.linker.(*llndkHeadersDecorator)
	if llndk || llndkHeader || (c.UseVndk() && c.HasVendorVariant()) {
		// .vendor.{version} suffix is added for vendor variant or .product.{version} suffix is
		// added for product variant only when we have vendor and product variants with core
		// variant. The suffix is not added for vendor-only or product-only module.
		c.Properties.SubName += c.getNameSuffixWithVndkVersion(actx)
	} else if _, ok := c.linker.(*vndkPrebuiltLibraryDecorator); ok {
		// .vendor suffix is added for backward compatibility with VNDK snapshot whose names with
		// such suffixes are already hard-coded in prebuilts/vndk/.../Android.bp.
		c.Properties.SubName += vendorSuffix
	} else if c.InRamdisk() && !c.OnlyInRamdisk() {
		c.Properties.SubName += ramdiskSuffix
	} else if c.InVendorRamdisk() && !c.OnlyInVendorRamdisk() {
		c.Properties.SubName += vendorRamdiskSuffix
	} else if c.InRecovery() && !c.OnlyInRecovery() {
		c.Properties.SubName += recoverySuffix
	} else if c.IsSdkVariant() && (c.Properties.SdkAndPlatformVariantVisibleToMake || c.SplitPerApiLevel()) {
		c.Properties.SubName += sdkSuffix
		if c.SplitPerApiLevel() {
			c.Properties.SubName += "." + c.SdkVersion()
		}
	}

	ctx := &moduleContext{
		ModuleContext: actx,
		moduleContextImpl: moduleContextImpl{
			mod: c,
		},
	}
	ctx.ctx = ctx

	deps := c.depsToPaths(ctx)
	if ctx.Failed() {
		return
	}

	if c.Properties.Clang != nil && *c.Properties.Clang == false {
		ctx.PropertyErrorf("clang", "false (GCC) is no longer supported")
	}

	flags := Flags{
		Toolchain: c.toolchain(ctx),
		EmitXrefs: ctx.Config().EmitXrefRules(),
	}
	if c.compiler != nil {
		flags = c.compiler.compilerFlags(ctx, flags, deps)
	}
	if c.linker != nil {
		flags = c.linker.linkerFlags(ctx, flags)
	}
	if c.stl != nil {
		flags = c.stl.flags(ctx, flags)
	}
	if c.sanitize != nil {
		flags = c.sanitize.flags(ctx, flags)
	}
	if c.coverage != nil {
		flags, deps = c.coverage.flags(ctx, flags, deps)
	}
	if c.lto != nil {
		flags = c.lto.flags(ctx, flags)
	}
	if c.pgo != nil {
		flags = c.pgo.flags(ctx, flags)
	}
	for _, feature := range c.features {
		flags = feature.flags(ctx, flags)
	}
	if ctx.Failed() {
		return
	}

	flags.Local.CFlags, _ = filterList(flags.Local.CFlags, config.IllegalFlags)
	flags.Local.CppFlags, _ = filterList(flags.Local.CppFlags, config.IllegalFlags)
	flags.Local.ConlyFlags, _ = filterList(flags.Local.ConlyFlags, config.IllegalFlags)

	flags.Local.CommonFlags = append(flags.Local.CommonFlags, deps.Flags...)

	for _, dir := range deps.IncludeDirs {
		flags.Local.CommonFlags = append(flags.Local.CommonFlags, "-I"+dir.String())
	}
	for _, dir := range deps.SystemIncludeDirs {
		flags.Local.CommonFlags = append(flags.Local.CommonFlags, "-isystem "+dir.String())
	}

	c.flags = flags
	// We need access to all the flags seen by a source file.
	if c.sabi != nil {
		flags = c.sabi.flags(ctx, flags)
	}

	flags.AssemblerWithCpp = inList("-xassembler-with-cpp", flags.Local.AsFlags)

	// Optimization to reduce size of build.ninja
	// Replace the long list of flags for each file with a module-local variable
	ctx.Variable(pctx, "cflags", strings.Join(flags.Local.CFlags, " "))
	ctx.Variable(pctx, "cppflags", strings.Join(flags.Local.CppFlags, " "))
	ctx.Variable(pctx, "asflags", strings.Join(flags.Local.AsFlags, " "))
	flags.Local.CFlags = []string{"$cflags"}
	flags.Local.CppFlags = []string{"$cppflags"}
	flags.Local.AsFlags = []string{"$asflags"}

	var objs Objects
	if c.compiler != nil {
		objs = c.compiler.compile(ctx, flags, deps)
		if ctx.Failed() {
			return
		}
		c.kytheFiles = objs.kytheFiles
	}

	if c.linker != nil {
		outputFile := c.linker.link(ctx, flags, deps, objs)
		if ctx.Failed() {
			return
		}
		c.outputFile = android.OptionalPathForPath(outputFile)

		// If a lib is directly included in any of the APEXes, unhide the stubs
		// variant having the latest version gets visible to make. In addition,
		// the non-stubs variant is renamed to <libname>.bootstrap. This is to
		// force anything in the make world to link against the stubs library.
		// (unless it is explicitly referenced via .bootstrap suffix or the
		// module is marked with 'bootstrap: true').
		if c.HasStubsVariants() && c.AnyVariantDirectlyInAnyApex() && !c.InRamdisk() &&
			!c.InRecovery() && !c.UseVndk() && !c.static() && !c.isCoverageVariant() &&
			c.IsStubs() && !c.InVendorRamdisk() {
			c.Properties.HideFromMake = false // unhide
			// Note: this is still non-installable
		}

		// glob exported headers for snapshot, if BOARD_VNDK_VERSION is current.
		if i, ok := c.linker.(snapshotLibraryInterface); ok && ctx.DeviceConfig().VndkVersion() == "current" {
			if isSnapshotAware(ctx, c, apexInfo) {
				i.collectHeadersForSnapshot(ctx)
			}
		}
	}

	if c.installable(apexInfo) {
		c.installer.install(ctx, c.outputFile.Path())
		if ctx.Failed() {
			return
		}
	} else if !proptools.BoolDefault(c.Properties.Installable, true) {
		// If the module has been specifically configure to not be installed then
		// skip the installation as otherwise it will break when running inside make
		// as the output path to install will not be specified. Not all uninstallable
		// modules can skip installation as some are needed for resolving make side
		// dependencies.
		c.SkipInstall()
	}
}

func (c *Module) toolchain(ctx android.BaseModuleContext) config.Toolchain {
	if c.cachedToolchain == nil {
		c.cachedToolchain = config.FindToolchain(ctx.Os(), ctx.Arch())
	}
	return c.cachedToolchain
}

func (c *Module) begin(ctx BaseModuleContext) {
	if c.compiler != nil {
		c.compiler.compilerInit(ctx)
	}
	if c.linker != nil {
		c.linker.linkerInit(ctx)
	}
	if c.stl != nil {
		c.stl.begin(ctx)
	}
	if c.sanitize != nil {
		c.sanitize.begin(ctx)
	}
	if c.coverage != nil {
		c.coverage.begin(ctx)
	}
	if c.sabi != nil {
		c.sabi.begin(ctx)
	}
	if c.vndkdep != nil {
		c.vndkdep.begin(ctx)
	}
	if c.lto != nil {
		c.lto.begin(ctx)
	}
	if c.pgo != nil {
		c.pgo.begin(ctx)
	}
	for _, feature := range c.features {
		feature.begin(ctx)
	}
	if ctx.useSdk() && c.IsSdkVariant() {
		version, err := nativeApiLevelFromUser(ctx, ctx.sdkVersion())
		if err != nil {
			ctx.PropertyErrorf("sdk_version", err.Error())
			c.Properties.Sdk_version = nil
		} else {
			c.Properties.Sdk_version = StringPtr(version.String())
		}
	}
}

func (c *Module) deps(ctx DepsContext) Deps {
	deps := Deps{}

	if c.compiler != nil {
		deps = c.compiler.compilerDeps(ctx, deps)
	}
	// Add the PGO dependency (the clang_rt.profile runtime library), which
	// sometimes depends on symbols from libgcc, before libgcc gets added
	// in linkerDeps().
	if c.pgo != nil {
		deps = c.pgo.deps(ctx, deps)
	}
	if c.linker != nil {
		deps = c.linker.linkerDeps(ctx, deps)
	}
	if c.stl != nil {
		deps = c.stl.deps(ctx, deps)
	}
	if c.sanitize != nil {
		deps = c.sanitize.deps(ctx, deps)
	}
	if c.coverage != nil {
		deps = c.coverage.deps(ctx, deps)
	}
	if c.sabi != nil {
		deps = c.sabi.deps(ctx, deps)
	}
	if c.vndkdep != nil {
		deps = c.vndkdep.deps(ctx, deps)
	}
	if c.lto != nil {
		deps = c.lto.deps(ctx, deps)
	}
	for _, feature := range c.features {
		deps = feature.deps(ctx, deps)
	}

	deps.WholeStaticLibs = android.LastUniqueStrings(deps.WholeStaticLibs)
	deps.StaticLibs = android.LastUniqueStrings(deps.StaticLibs)
	deps.LateStaticLibs = android.LastUniqueStrings(deps.LateStaticLibs)
	deps.SharedLibs = android.LastUniqueStrings(deps.SharedLibs)
	deps.LateSharedLibs = android.LastUniqueStrings(deps.LateSharedLibs)
	deps.HeaderLibs = android.LastUniqueStrings(deps.HeaderLibs)
	deps.RuntimeLibs = android.LastUniqueStrings(deps.RuntimeLibs)

	for _, lib := range deps.ReexportSharedLibHeaders {
		if !inList(lib, deps.SharedLibs) {
			ctx.PropertyErrorf("export_shared_lib_headers", "Shared library not in shared_libs: '%s'", lib)
		}
	}

	for _, lib := range deps.ReexportStaticLibHeaders {
		if !inList(lib, deps.StaticLibs) {
			ctx.PropertyErrorf("export_static_lib_headers", "Static library not in static_libs: '%s'", lib)
		}
	}

	for _, lib := range deps.ReexportHeaderLibHeaders {
		if !inList(lib, deps.HeaderLibs) {
			ctx.PropertyErrorf("export_header_lib_headers", "Header library not in header_libs: '%s'", lib)
		}
	}

	for _, gen := range deps.ReexportGeneratedHeaders {
		if !inList(gen, deps.GeneratedHeaders) {
			ctx.PropertyErrorf("export_generated_headers", "Generated header module not in generated_headers: '%s'", gen)
		}
	}

	return deps
}

func (c *Module) beginMutator(actx android.BottomUpMutatorContext) {
	ctx := &baseModuleContext{
		BaseModuleContext: actx,
		moduleContextImpl: moduleContextImpl{
			mod: c,
		},
	}
	ctx.ctx = ctx

	c.begin(ctx)
}

// Split name#version into name and version
func StubsLibNameAndVersion(name string) (string, string) {
	if sharp := strings.LastIndex(name, "#"); sharp != -1 && sharp != len(name)-1 {
		version := name[sharp+1:]
		libname := name[:sharp]
		return libname, version
	}
	return name, ""
}

func GetCrtVariations(ctx android.BottomUpMutatorContext,
	m LinkableInterface) []blueprint.Variation {
	if ctx.Os() != android.Android {
		return nil
	}
	if m.UseSdk() {
		return []blueprint.Variation{
			{Mutator: "sdk", Variation: "sdk"},
			{Mutator: "version", Variation: m.SdkVersion()},
		}
	}
	return []blueprint.Variation{
		{Mutator: "sdk", Variation: ""},
	}
}

func (c *Module) addSharedLibDependenciesWithVersions(ctx android.BottomUpMutatorContext,
	variations []blueprint.Variation, depTag libraryDependencyTag, name, version string, far bool) {

	variations = append([]blueprint.Variation(nil), variations...)

	if version != "" && CanBeOrLinkAgainstVersionVariants(c) {
		// Version is explicitly specified. i.e. libFoo#30
		variations = append(variations, blueprint.Variation{Mutator: "version", Variation: version})
		depTag.explicitlyVersioned = true
	}

	if far {
		ctx.AddFarVariationDependencies(variations, depTag, name)
	} else {
		ctx.AddVariationDependencies(variations, depTag, name)
	}
}

func (c *Module) DepsMutator(actx android.BottomUpMutatorContext) {
	if !c.Enabled() {
		return
	}

	ctx := &depsContext{
		BottomUpMutatorContext: actx,
		moduleContextImpl: moduleContextImpl{
			mod: c,
		},
	}
	ctx.ctx = ctx

	deps := c.deps(ctx)

	c.Properties.AndroidMkSystemSharedLibs = deps.SystemSharedLibs

	variantNdkLibs := []string{}
	variantLateNdkLibs := []string{}
	if ctx.Os() == android.Android {
		// rewriteLibs takes a list of names of shared libraries and scans it for three types
		// of names:
		//
		// 1. Name of an NDK library that refers to a prebuilt module.
		//    For each of these, it adds the name of the prebuilt module (which will be in
		//    prebuilts/ndk) to the list of nonvariant libs.
		// 2. Name of an NDK library that refers to an ndk_library module.
		//    For each of these, it adds the name of the ndk_library module to the list of
		//    variant libs.
		// 3. Anything else (so anything that isn't an NDK library).
		//    It adds these to the nonvariantLibs list.
		//
		// The caller can then know to add the variantLibs dependencies differently from the
		// nonvariantLibs

		vendorPublicLibraries := vendorPublicLibraries(actx.Config())
		vendorSnapshotSharedLibs := vendorSnapshotSharedLibs(actx.Config())

		rewriteVendorLibs := func(lib string) string {
			if isLlndkLibrary(lib, ctx.Config()) {
				return lib + llndkLibrarySuffix
			}

			// only modules with BOARD_VNDK_VERSION uses snapshot.
			if c.VndkVersion() != actx.DeviceConfig().VndkVersion() {
				return lib
			}

			if snapshot, ok := vendorSnapshotSharedLibs.get(lib, actx.Arch().ArchType); ok {
				return snapshot
			}

			return lib
		}

		rewriteLibs := func(list []string) (nonvariantLibs []string, variantLibs []string) {
			variantLibs = []string{}
			nonvariantLibs = []string{}
			for _, entry := range list {
				// strip #version suffix out
				name, _ := StubsLibNameAndVersion(entry)
				if ctx.useSdk() && inList(name, ndkKnownLibs) {
					variantLibs = append(variantLibs, name+ndkLibrarySuffix)
				} else if ctx.useVndk() {
					nonvariantLibs = append(nonvariantLibs, rewriteVendorLibs(entry))
				} else if (ctx.Platform() || ctx.ProductSpecific()) && inList(name, *vendorPublicLibraries) {
					vendorPublicLib := name + vendorPublicLibrarySuffix
					if actx.OtherModuleExists(vendorPublicLib) {
						nonvariantLibs = append(nonvariantLibs, vendorPublicLib)
					} else {
						// This can happen if vendor_public_library module is defined in a
						// namespace that isn't visible to the current module. In that case,
						// link to the original library.
						nonvariantLibs = append(nonvariantLibs, name)
					}
				} else {
					// put name#version back
					nonvariantLibs = append(nonvariantLibs, entry)
				}
			}
			return nonvariantLibs, variantLibs
		}

		deps.SharedLibs, variantNdkLibs = rewriteLibs(deps.SharedLibs)
		deps.LateSharedLibs, variantLateNdkLibs = rewriteLibs(deps.LateSharedLibs)
		deps.ReexportSharedLibHeaders, _ = rewriteLibs(deps.ReexportSharedLibHeaders)
		if ctx.useVndk() {
			for idx, lib := range deps.RuntimeLibs {
				deps.RuntimeLibs[idx] = rewriteVendorLibs(lib)
			}
		}
	}

	buildStubs := false
	if versioned, ok := c.linker.(versionedInterface); ok {
		if versioned.buildStubs() {
			buildStubs = true
		}
	}

	rewriteSnapshotLibs := func(lib string, snapshotMap *snapshotMap) string {
		// only modules with BOARD_VNDK_VERSION uses snapshot.
		if c.VndkVersion() != actx.DeviceConfig().VndkVersion() {
			return lib
		}

		if snapshot, ok := snapshotMap.get(lib, actx.Arch().ArchType); ok {
			return snapshot
		}

		return lib
	}

	vendorSnapshotHeaderLibs := vendorSnapshotHeaderLibs(actx.Config())
	for _, lib := range deps.HeaderLibs {
		depTag := libraryDependencyTag{Kind: headerLibraryDependency}
		if inList(lib, deps.ReexportHeaderLibHeaders) {
			depTag.reexportFlags = true
		}

		lib = rewriteSnapshotLibs(lib, vendorSnapshotHeaderLibs)

		if buildStubs {
			actx.AddFarVariationDependencies(append(ctx.Target().Variations(), c.ImageVariation()),
				depTag, lib)
		} else {
			actx.AddVariationDependencies(nil, depTag, lib)
		}
	}

	if buildStubs {
		// Stubs lib does not have dependency to other static/shared libraries.
		// Don't proceed.
		return
	}

	syspropImplLibraries := syspropImplLibraries(actx.Config())
	vendorSnapshotStaticLibs := vendorSnapshotStaticLibs(actx.Config())

	for _, lib := range deps.WholeStaticLibs {
		depTag := libraryDependencyTag{Kind: staticLibraryDependency, wholeStatic: true, reexportFlags: true}
		if impl, ok := syspropImplLibraries[lib]; ok {
			lib = impl
		}

		lib = rewriteSnapshotLibs(lib, vendorSnapshotStaticLibs)

		actx.AddVariationDependencies([]blueprint.Variation{
			{Mutator: "link", Variation: "static"},
		}, depTag, lib)
	}

	for _, lib := range deps.StaticLibs {
		depTag := libraryDependencyTag{Kind: staticLibraryDependency}
		if inList(lib, deps.ReexportStaticLibHeaders) {
			depTag.reexportFlags = true
		}

		if impl, ok := syspropImplLibraries[lib]; ok {
			lib = impl
		}

		lib = rewriteSnapshotLibs(lib, vendorSnapshotStaticLibs)

		actx.AddVariationDependencies([]blueprint.Variation{
			{Mutator: "link", Variation: "static"},
		}, depTag, lib)
	}

	// staticUnwinderDep is treated as staticDep for Q apexes
	// so that native libraries/binaries are linked with static unwinder
	// because Q libc doesn't have unwinder APIs
	if deps.StaticUnwinderIfLegacy {
		depTag := libraryDependencyTag{Kind: staticLibraryDependency, staticUnwinder: true}
		actx.AddVariationDependencies([]blueprint.Variation{
			{Mutator: "link", Variation: "static"},
		}, depTag, rewriteSnapshotLibs(staticUnwinder(actx), vendorSnapshotStaticLibs))
	}

	for _, lib := range deps.LateStaticLibs {
		depTag := libraryDependencyTag{Kind: staticLibraryDependency, Order: lateLibraryDependency}
		actx.AddVariationDependencies([]blueprint.Variation{
			{Mutator: "link", Variation: "static"},
		}, depTag, rewriteSnapshotLibs(lib, vendorSnapshotStaticLibs))
	}

	// shared lib names without the #version suffix
	var sharedLibNames []string

	for _, lib := range deps.SharedLibs {
		depTag := libraryDependencyTag{Kind: sharedLibraryDependency}
		if inList(lib, deps.ReexportSharedLibHeaders) {
			depTag.reexportFlags = true
		}

		if impl, ok := syspropImplLibraries[lib]; ok {
			lib = impl
		}

		name, version := StubsLibNameAndVersion(lib)
		sharedLibNames = append(sharedLibNames, name)

		variations := []blueprint.Variation{
			{Mutator: "link", Variation: "shared"},
		}
		c.addSharedLibDependenciesWithVersions(ctx, variations, depTag, name, version, false)
	}

	for _, lib := range deps.LateSharedLibs {
		if inList(lib, sharedLibNames) {
			// This is to handle the case that some of the late shared libs (libc, libdl, libm, ...)
			// are added also to SharedLibs with version (e.g., libc#10). If not skipped, we will be
			// linking against both the stubs lib and the non-stubs lib at the same time.
			continue
		}
		depTag := libraryDependencyTag{Kind: sharedLibraryDependency, Order: lateLibraryDependency}
		variations := []blueprint.Variation{
			{Mutator: "link", Variation: "shared"},
		}
		c.addSharedLibDependenciesWithVersions(ctx, variations, depTag, lib, "", false)
	}

	actx.AddVariationDependencies([]blueprint.Variation{
		{Mutator: "link", Variation: "shared"},
	}, dataLibDepTag, deps.DataLibs...)

	actx.AddVariationDependencies([]blueprint.Variation{
		{Mutator: "link", Variation: "shared"},
	}, runtimeDepTag, deps.RuntimeLibs...)

	actx.AddDependency(c, genSourceDepTag, deps.GeneratedSources...)

	for _, gen := range deps.GeneratedHeaders {
		depTag := genHeaderDepTag
		if inList(gen, deps.ReexportGeneratedHeaders) {
			depTag = genHeaderExportDepTag
		}
		actx.AddDependency(c, depTag, gen)
	}

	vendorSnapshotObjects := vendorSnapshotObjects(actx.Config())

	crtVariations := GetCrtVariations(ctx, c)
	actx.AddVariationDependencies(crtVariations, objDepTag, deps.ObjFiles...)
	if deps.CrtBegin != "" {
		actx.AddVariationDependencies(crtVariations, CrtBeginDepTag,
			rewriteSnapshotLibs(deps.CrtBegin, vendorSnapshotObjects))
	}
	if deps.CrtEnd != "" {
		actx.AddVariationDependencies(crtVariations, CrtEndDepTag,
			rewriteSnapshotLibs(deps.CrtEnd, vendorSnapshotObjects))
	}
	if deps.LinkerFlagsFile != "" {
		actx.AddDependency(c, linkerFlagsDepTag, deps.LinkerFlagsFile)
	}
	if deps.DynamicLinker != "" {
		actx.AddDependency(c, dynamicLinkerDepTag, deps.DynamicLinker)
	}

	version := ctx.sdkVersion()

	ndkStubDepTag := libraryDependencyTag{Kind: sharedLibraryDependency, ndk: true, makeSuffix: "." + version}
	actx.AddVariationDependencies([]blueprint.Variation{
		{Mutator: "version", Variation: version},
		{Mutator: "link", Variation: "shared"},
	}, ndkStubDepTag, variantNdkLibs...)

	ndkLateStubDepTag := libraryDependencyTag{Kind: sharedLibraryDependency, Order: lateLibraryDependency, ndk: true, makeSuffix: "." + version}
	actx.AddVariationDependencies([]blueprint.Variation{
		{Mutator: "version", Variation: version},
		{Mutator: "link", Variation: "shared"},
	}, ndkLateStubDepTag, variantLateNdkLibs...)

	if vndkdep := c.vndkdep; vndkdep != nil {
		if vndkdep.isVndkExt() {
			actx.AddVariationDependencies([]blueprint.Variation{
				c.ImageVariation(),
				{Mutator: "link", Variation: "shared"},
			}, vndkExtDepTag, vndkdep.getVndkExtendsModuleName())
		}
	}
}

func BeginMutator(ctx android.BottomUpMutatorContext) {
	if c, ok := ctx.Module().(*Module); ok && c.Enabled() {
		c.beginMutator(ctx)
	}
}

// Whether a module can link to another module, taking into
// account NDK linking.
func checkLinkType(ctx android.BaseModuleContext, from LinkableInterface, to LinkableInterface,
	tag blueprint.DependencyTag) {

	switch t := tag.(type) {
	case dependencyTag:
		if t != vndkExtDepTag {
			return
		}
	case libraryDependencyTag:
	default:
		return
	}

	if from.Module().Target().Os != android.Android {
		// Host code is not restricted
		return
	}

	// VNDK is cc.Module supported only for now.
	if ccFrom, ok := from.(*Module); ok && from.UseVndk() {
		// Though vendor code is limited by the vendor mutator,
		// each vendor-available module needs to check
		// link-type for VNDK.
		if ccTo, ok := to.(*Module); ok {
			if ccFrom.vndkdep != nil {
				ccFrom.vndkdep.vndkCheckLinkType(ctx, ccTo, tag)
			}
		} else {
			ctx.ModuleErrorf("Attempting to link VNDK cc.Module with unsupported module type")
		}
		return
	}
	if from.SdkVersion() == "" {
		// Platform code can link to anything
		return
	}
	if from.InRamdisk() {
		// Ramdisk code is not NDK
		return
	}
	if from.InVendorRamdisk() {
		// Vendor ramdisk code is not NDK
		return
	}
	if from.InRecovery() {
		// Recovery code is not NDK
		return
	}
	if c, ok := to.(*Module); ok {
		if c.ToolchainLibrary() {
			// These are always allowed
			return
		}
		if c.NdkPrebuiltStl() {
			// These are allowed, but they don't set sdk_version
			return
		}
		if c.StubDecorator() {
			// These aren't real libraries, but are the stub shared libraries that are included in
			// the NDK.
			return
		}
	}

	if strings.HasPrefix(ctx.ModuleName(), "libclang_rt.") && to.Module().Name() == "libc++" {
		// Bug: http://b/121358700 - Allow libclang_rt.* shared libraries (with sdk_version)
		// to link to libc++ (non-NDK and without sdk_version).
		return
	}

	if to.SdkVersion() == "" {
		// NDK code linking to platform code is never okay.
		ctx.ModuleErrorf("depends on non-NDK-built library %q",
			ctx.OtherModuleName(to.Module()))
		return
	}

	// At this point we know we have two NDK libraries, but we need to
	// check that we're not linking against anything built against a higher
	// API level, as it is only valid to link against older or equivalent
	// APIs.

	// Current can link against anything.
	if from.SdkVersion() != "current" {
		// Otherwise we need to check.
		if to.SdkVersion() == "current" {
			// Current can't be linked against by anything else.
			ctx.ModuleErrorf("links %q built against newer API version %q",
				ctx.OtherModuleName(to.Module()), "current")
		} else {
			fromApi, err := strconv.Atoi(from.SdkVersion())
			if err != nil {
				ctx.PropertyErrorf("sdk_version",
					"Invalid sdk_version value (must be int or current): %q",
					from.SdkVersion())
			}
			toApi, err := strconv.Atoi(to.SdkVersion())
			if err != nil {
				ctx.PropertyErrorf("sdk_version",
					"Invalid sdk_version value (must be int or current): %q",
					to.SdkVersion())
			}

			if toApi > fromApi {
				ctx.ModuleErrorf("links %q built against newer API version %q",
					ctx.OtherModuleName(to.Module()), to.SdkVersion())
			}
		}
	}

	// Also check that the two STL choices are compatible.
	fromStl := from.SelectedStl()
	toStl := to.SelectedStl()
	if fromStl == "" || toStl == "" {
		// Libraries that don't use the STL are unrestricted.
	} else if fromStl == "ndk_system" || toStl == "ndk_system" {
		// We can be permissive with the system "STL" since it is only the C++
		// ABI layer, but in the future we should make sure that everyone is
		// using either libc++ or nothing.
	} else if getNdkStlFamily(from) != getNdkStlFamily(to) {
		ctx.ModuleErrorf("uses %q and depends on %q which uses incompatible %q",
			from.SelectedStl(), ctx.OtherModuleName(to.Module()),
			to.SelectedStl())
	}
}

func checkLinkTypeMutator(ctx android.BottomUpMutatorContext) {
	if c, ok := ctx.Module().(*Module); ok {
		ctx.VisitDirectDeps(func(dep android.Module) {
			depTag := ctx.OtherModuleDependencyTag(dep)
			ccDep, ok := dep.(LinkableInterface)
			if ok {
				checkLinkType(ctx, c, ccDep, depTag)
			}
		})
	}
}

// Tests whether the dependent library is okay to be double loaded inside a single process.
// If a library has a vendor variant and is a (transitive) dependency of an LLNDK library,
// it is subject to be double loaded. Such lib should be explicitly marked as double_loadable: true
// or as vndk-sp (vndk: { enabled: true, support_system_process: true}).
func checkDoubleLoadableLibraries(ctx android.TopDownMutatorContext) {
	check := func(child, parent android.Module) bool {
		to, ok := child.(*Module)
		if !ok {
			return false
		}

		if lib, ok := to.linker.(*libraryDecorator); !ok || !lib.shared() {
			return false
		}

		// Even if target lib has no vendor variant, keep checking dependency graph
		// in case it depends on vendor_available but not double_loadable transtively.
		if !to.HasVendorVariant() {
			return true
		}

		if to.isVndkSp() || to.isLlndk(ctx.Config()) || Bool(to.VendorProperties.Double_loadable) {
			return false
		}

		var stringPath []string
		for _, m := range ctx.GetWalkPath() {
			stringPath = append(stringPath, m.Name())
		}
		ctx.ModuleErrorf("links a library %q which is not LL-NDK, "+
			"VNDK-SP, or explicitly marked as 'double_loadable:true'. "+
			"(dependency: %s)", ctx.OtherModuleName(to), strings.Join(stringPath, " -> "))
		return false
	}
	if module, ok := ctx.Module().(*Module); ok {
		if lib, ok := module.linker.(*libraryDecorator); ok && lib.shared() {
			if module.isLlndk(ctx.Config()) || Bool(module.VendorProperties.Double_loadable) {
				ctx.WalkDeps(check)
			}
		}
	}
}

// Returns the highest version which is <= maxSdkVersion.
// For example, with maxSdkVersion is 10 and versionList is [9,11]
// it returns 9 as string.  The list of stubs must be in order from
// oldest to newest.
func (c *Module) chooseSdkVersion(ctx android.PathContext, stubsInfo []SharedLibraryStubsInfo,
	maxSdkVersion android.ApiLevel) (SharedLibraryStubsInfo, error) {

	for i := range stubsInfo {
		stubInfo := stubsInfo[len(stubsInfo)-i-1]
		var ver android.ApiLevel
		if stubInfo.Version == "" {
			ver = android.FutureApiLevel
		} else {
			var err error
			ver, err = android.ApiLevelFromUser(ctx, stubInfo.Version)
			if err != nil {
				return SharedLibraryStubsInfo{}, err
			}
		}
		if ver.LessThanOrEqualTo(maxSdkVersion) {
			return stubInfo, nil
		}
	}
	var versionList []string
	for _, stubInfo := range stubsInfo {
		versionList = append(versionList, stubInfo.Version)
	}
	return SharedLibraryStubsInfo{}, fmt.Errorf("not found a version(<=%s) in versionList: %v", maxSdkVersion.String(), versionList)
}

// Convert dependencies to paths.  Returns a PathDeps containing paths
func (c *Module) depsToPaths(ctx android.ModuleContext) PathDeps {
	var depPaths PathDeps

	var directStaticDeps []StaticLibraryInfo
	var directSharedDeps []SharedLibraryInfo

	reexportExporter := func(exporter FlagExporterInfo) {
		depPaths.ReexportedDirs = append(depPaths.ReexportedDirs, exporter.IncludeDirs...)
		depPaths.ReexportedSystemDirs = append(depPaths.ReexportedSystemDirs, exporter.SystemIncludeDirs...)
		depPaths.ReexportedFlags = append(depPaths.ReexportedFlags, exporter.Flags...)
		depPaths.ReexportedDeps = append(depPaths.ReexportedDeps, exporter.Deps...)
		depPaths.ReexportedGeneratedHeaders = append(depPaths.ReexportedGeneratedHeaders, exporter.GeneratedHeaders...)
	}

	// For the dependency from platform to apex, use the latest stubs
	c.apexSdkVersion = android.FutureApiLevel
	apexInfo := ctx.Provider(android.ApexInfoProvider).(android.ApexInfo)
	if !apexInfo.IsForPlatform() {
		c.apexSdkVersion = apexInfo.MinSdkVersion(ctx)
	}

	if android.InList("hwaddress", ctx.Config().SanitizeDevice()) {
		// In hwasan build, we override apexSdkVersion to the FutureApiLevel(10000)
		// so that even Q(29/Android10) apexes could use the dynamic unwinder by linking the newer stubs(e.g libc(R+)).
		// (b/144430859)
		c.apexSdkVersion = android.FutureApiLevel
	}

	ctx.VisitDirectDeps(func(dep android.Module) {
		depName := ctx.OtherModuleName(dep)
		depTag := ctx.OtherModuleDependencyTag(dep)

		ccDep, ok := dep.(LinkableInterface)
		if !ok {

			// handling for a few module types that aren't cc Module but that are also supported
			switch depTag {
			case genSourceDepTag:
				if genRule, ok := dep.(genrule.SourceFileGenerator); ok {
					depPaths.GeneratedSources = append(depPaths.GeneratedSources,
						genRule.GeneratedSourceFiles()...)
				} else {
					ctx.ModuleErrorf("module %q is not a gensrcs or genrule", depName)
				}
				// Support exported headers from a generated_sources dependency
				fallthrough
			case genHeaderDepTag, genHeaderExportDepTag:
				if genRule, ok := dep.(genrule.SourceFileGenerator); ok {
					depPaths.GeneratedDeps = append(depPaths.GeneratedDeps,
						genRule.GeneratedDeps()...)
					dirs := genRule.GeneratedHeaderDirs()
					depPaths.IncludeDirs = append(depPaths.IncludeDirs, dirs...)
					if depTag == genHeaderExportDepTag {
						depPaths.ReexportedDirs = append(depPaths.ReexportedDirs, dirs...)
						depPaths.ReexportedGeneratedHeaders = append(depPaths.ReexportedGeneratedHeaders,
							genRule.GeneratedSourceFiles()...)
						depPaths.ReexportedDeps = append(depPaths.ReexportedDeps, genRule.GeneratedDeps()...)
						// Add these re-exported flags to help header-abi-dumper to infer the abi exported by a library.
						c.sabi.Properties.ReexportedIncludes = append(c.sabi.Properties.ReexportedIncludes, dirs.Strings()...)

					}
				} else {
					ctx.ModuleErrorf("module %q is not a genrule", depName)
				}
			case linkerFlagsDepTag:
				if genRule, ok := dep.(genrule.SourceFileGenerator); ok {
					files := genRule.GeneratedSourceFiles()
					if len(files) == 1 {
						depPaths.LinkerFlagsFile = android.OptionalPathForPath(files[0])
					} else if len(files) > 1 {
						ctx.ModuleErrorf("module %q can only generate a single file if used for a linker flag file", depName)
					}
				} else {
					ctx.ModuleErrorf("module %q is not a genrule", depName)
				}
			}
			return
		}

		if depTag == android.ProtoPluginDepTag {
			return
		}
		if depTag == llndkImplDep {
			return
		}

		if dep.Target().Os != ctx.Os() {
			ctx.ModuleErrorf("OS mismatch between %q and %q", ctx.ModuleName(), depName)
			return
		}
		if dep.Target().Arch.ArchType != ctx.Arch().ArchType {
			ctx.ModuleErrorf("Arch mismatch between %q(%v) and %q(%v)",
				ctx.ModuleName(), ctx.Arch().ArchType, depName, dep.Target().Arch.ArchType)
			return
		}

		if depTag == reuseObjTag {
			// Skip reused objects for stub libraries, they use their own stub object file instead.
			// The reuseObjTag dependency still exists because the LinkageMutator runs before the
			// version mutator, so the stubs variant is created from the shared variant that
			// already has the reuseObjTag dependency on the static variant.
			if !c.library.buildStubs() {
				staticAnalogue := ctx.OtherModuleProvider(dep, StaticLibraryInfoProvider).(StaticLibraryInfo)
				objs := staticAnalogue.ReuseObjects
				depPaths.Objs = depPaths.Objs.Append(objs)
				depExporterInfo := ctx.OtherModuleProvider(dep, FlagExporterInfoProvider).(FlagExporterInfo)
				reexportExporter(depExporterInfo)
			}
			return
		}

		linkFile := ccDep.OutputFile()

		if libDepTag, ok := depTag.(libraryDependencyTag); ok {
			// Only use static unwinder for legacy (min_sdk_version = 29) apexes (b/144430859)
			if libDepTag.staticUnwinder && c.apexSdkVersion.GreaterThan(android.SdkVersion_Android10) {
				return
			}

			depExporterInfo := ctx.OtherModuleProvider(dep, FlagExporterInfoProvider).(FlagExporterInfo)

			var ptr *android.Paths
			var depPtr *android.Paths

			depFile := android.OptionalPath{}

			switch {
			case libDepTag.header():
				// nothing
			case libDepTag.shared():
				if !ctx.OtherModuleHasProvider(dep, SharedLibraryInfoProvider) {
					if !ctx.Config().AllowMissingDependencies() {
						ctx.ModuleErrorf("module %q is not a shared library", depName)
					} else {
						ctx.AddMissingDependencies([]string{depName})
					}
					return
				}
				sharedLibraryInfo := ctx.OtherModuleProvider(dep, SharedLibraryInfoProvider).(SharedLibraryInfo)
				sharedLibraryStubsInfo := ctx.OtherModuleProvider(dep, SharedLibraryImplementationStubsInfoProvider).(SharedLibraryImplementationStubsInfo)

				if !libDepTag.explicitlyVersioned && len(sharedLibraryStubsInfo.SharedLibraryStubsInfos) > 0 {
					useStubs := false

					if lib := moduleLibraryInterface(dep); lib.buildStubs() && c.UseVndk() { // LLNDK
						if !apexInfo.IsForPlatform() {
							// For platform libraries, use current version of LLNDK
							// If this is for use_vendor apex we will apply the same rules
							// of apex sdk enforcement below to choose right version.
							useStubs = true
						}
					} else if apexInfo.IsForPlatform() {
						// If not building for APEX, use stubs only when it is from
						// an APEX (and not from platform)
						// However, for host, ramdisk, vendor_ramdisk, recovery or bootstrap modules,
						// always link to non-stub variant
						useStubs = dep.(android.ApexModule).AnyVariantDirectlyInAnyApex() && !c.bootstrap()
						// Another exception: if this module is bundled with an APEX, then
						// it is linked with the non-stub variant of a module in the APEX
						// as if this is part of the APEX.
						testFor := ctx.Provider(android.ApexTestForInfoProvider).(android.ApexTestForInfo)
						for _, apexContents := range testFor.ApexContents {
							if apexContents.DirectlyInApex(depName) {
								useStubs = false
								break
							}
						}
					} else {
						// If building for APEX, use stubs when the parent is in any APEX that
						// the child is not in.
						useStubs = !android.DirectlyInAllApexes(apexInfo, depName)
					}

					// when to use (unspecified) stubs, check min_sdk_version and choose the right one
					if useStubs {
						sharedLibraryStubsInfo, err :=
							c.chooseSdkVersion(ctx, sharedLibraryStubsInfo.SharedLibraryStubsInfos, c.apexSdkVersion)
						if err != nil {
							ctx.OtherModuleErrorf(dep, err.Error())
							return
						}
						sharedLibraryInfo = sharedLibraryStubsInfo.SharedLibraryInfo
						depExporterInfo = sharedLibraryStubsInfo.FlagExporterInfo
					}
				}

				linkFile = android.OptionalPathForPath(sharedLibraryInfo.SharedLibrary)
				depFile = sharedLibraryInfo.TableOfContents

				ptr = &depPaths.SharedLibs
				switch libDepTag.Order {
				case earlyLibraryDependency:
					ptr = &depPaths.EarlySharedLibs
					depPtr = &depPaths.EarlySharedLibsDeps
				case normalLibraryDependency:
					ptr = &depPaths.SharedLibs
					depPtr = &depPaths.SharedLibsDeps
					directSharedDeps = append(directSharedDeps, sharedLibraryInfo)
				case lateLibraryDependency:
					ptr = &depPaths.LateSharedLibs
					depPtr = &depPaths.LateSharedLibsDeps
				default:
					panic(fmt.Errorf("unexpected library dependency order %d", libDepTag.Order))
				}
			case libDepTag.static():
				if !ctx.OtherModuleHasProvider(dep, StaticLibraryInfoProvider) {
					if !ctx.Config().AllowMissingDependencies() {
						ctx.ModuleErrorf("module %q is not a static library", depName)
					} else {
						ctx.AddMissingDependencies([]string{depName})
					}
					return
				}
				staticLibraryInfo := ctx.OtherModuleProvider(dep, StaticLibraryInfoProvider).(StaticLibraryInfo)
				linkFile = android.OptionalPathForPath(staticLibraryInfo.StaticLibrary)
				if libDepTag.wholeStatic {
					ptr = &depPaths.WholeStaticLibs
					if len(staticLibraryInfo.Objects.objFiles) > 0 {
						depPaths.WholeStaticLibObjs = depPaths.WholeStaticLibObjs.Append(staticLibraryInfo.Objects)
					} else {
						// This case normally catches prebuilt static
						// libraries, but it can also occur when
						// AllowMissingDependencies is on and the
						// dependencies has no sources of its own
						// but has a whole_static_libs dependency
						// on a missing library.  We want to depend
						// on the .a file so that there is something
						// in the dependency tree that contains the
						// error rule for the missing transitive
						// dependency.
						depPaths.WholeStaticLibsFromPrebuilts = append(depPaths.WholeStaticLibsFromPrebuilts, linkFile.Path())
					}
				} else {
					switch libDepTag.Order {
					case earlyLibraryDependency:
						panic(fmt.Errorf("early static libs not suppported"))
					case normalLibraryDependency:
						// static dependencies will be handled separately so they can be ordered
						// using transitive dependencies.
						ptr = nil
						directStaticDeps = append(directStaticDeps, staticLibraryInfo)
					case lateLibraryDependency:
						ptr = &depPaths.LateStaticLibs
					default:
						panic(fmt.Errorf("unexpected library dependency order %d", libDepTag.Order))
					}
				}
			}

			if libDepTag.static() && !libDepTag.wholeStatic {
				if !ccDep.CcLibraryInterface() || !ccDep.Static() {
					ctx.ModuleErrorf("module %q not a static library", depName)
					return
				}

				// When combining coverage files for shared libraries and executables, coverage files
				// in static libraries act as if they were whole static libraries. The same goes for
				// source based Abi dump files.
				if c, ok := ccDep.(*Module); ok {
					staticLib := c.linker.(libraryInterface)
					depPaths.StaticLibObjs.coverageFiles = append(depPaths.StaticLibObjs.coverageFiles,
						staticLib.objs().coverageFiles...)
					depPaths.StaticLibObjs.sAbiDumpFiles = append(depPaths.StaticLibObjs.sAbiDumpFiles,
						staticLib.objs().sAbiDumpFiles...)
				} else {
					// Handle non-CC modules here
					depPaths.StaticLibObjs.coverageFiles = append(depPaths.StaticLibObjs.coverageFiles,
						ccDep.CoverageFiles()...)
				}
			}

			if ptr != nil {
				if !linkFile.Valid() {
					if !ctx.Config().AllowMissingDependencies() {
						ctx.ModuleErrorf("module %q missing output file", depName)
					} else {
						ctx.AddMissingDependencies([]string{depName})
					}
					return
				}
				*ptr = append(*ptr, linkFile.Path())
			}

			if depPtr != nil {
				dep := depFile
				if !dep.Valid() {
					dep = linkFile
				}
				*depPtr = append(*depPtr, dep.Path())
			}

			depPaths.IncludeDirs = append(depPaths.IncludeDirs, depExporterInfo.IncludeDirs...)
			depPaths.SystemIncludeDirs = append(depPaths.SystemIncludeDirs, depExporterInfo.SystemIncludeDirs...)
			depPaths.GeneratedDeps = append(depPaths.GeneratedDeps, depExporterInfo.Deps...)
			depPaths.Flags = append(depPaths.Flags, depExporterInfo.Flags...)

			if libDepTag.reexportFlags {
				reexportExporter(depExporterInfo)
				// Add these re-exported flags to help header-abi-dumper to infer the abi exported by a library.
				// Re-exported shared library headers must be included as well since they can help us with type information
				// about template instantiations (instantiated from their headers).
				// -isystem headers are not included since for bionic libraries, abi-filtering is taken care of by version
				// scripts.
				c.sabi.Properties.ReexportedIncludes = append(
					c.sabi.Properties.ReexportedIncludes, depExporterInfo.IncludeDirs.Strings()...)
			}

			makeLibName := c.makeLibName(ctx, ccDep, depName) + libDepTag.makeSuffix
			switch {
			case libDepTag.header():
				c.Properties.AndroidMkHeaderLibs = append(
					c.Properties.AndroidMkHeaderLibs, makeLibName)
			case libDepTag.shared():
				if lib := moduleLibraryInterface(dep); lib != nil {
					if lib.buildStubs() && dep.(android.ApexModule).InAnyApex() {
						// Add the dependency to the APEX(es) providing the library so that
						// m <module> can trigger building the APEXes as well.
						depApexInfo := ctx.OtherModuleProvider(dep, android.ApexInfoProvider).(android.ApexInfo)
						for _, an := range depApexInfo.InApexes {
							c.Properties.ApexesProvidingSharedLibs = append(
								c.Properties.ApexesProvidingSharedLibs, an)
						}
					}
				}

				// Note: the order of libs in this list is not important because
				// they merely serve as Make dependencies and do not affect this lib itself.
				c.Properties.AndroidMkSharedLibs = append(
					c.Properties.AndroidMkSharedLibs, makeLibName)
				// Record baseLibName for snapshots.
				c.Properties.SnapshotSharedLibs = append(c.Properties.SnapshotSharedLibs, baseLibName(depName))
			case libDepTag.static():
				if libDepTag.wholeStatic {
					c.Properties.AndroidMkWholeStaticLibs = append(
						c.Properties.AndroidMkWholeStaticLibs, makeLibName)
				} else {
					c.Properties.AndroidMkStaticLibs = append(
						c.Properties.AndroidMkStaticLibs, makeLibName)
				}
			}
		} else {
			switch depTag {
			case runtimeDepTag:
				c.Properties.AndroidMkRuntimeLibs = append(
					c.Properties.AndroidMkRuntimeLibs, c.makeLibName(ctx, ccDep, depName)+libDepTag.makeSuffix)
				// Record baseLibName for snapshots.
				c.Properties.SnapshotRuntimeLibs = append(c.Properties.SnapshotRuntimeLibs, baseLibName(depName))
			case objDepTag:
				depPaths.Objs.objFiles = append(depPaths.Objs.objFiles, linkFile.Path())
			case CrtBeginDepTag:
				depPaths.CrtBegin = linkFile
			case CrtEndDepTag:
				depPaths.CrtEnd = linkFile
			case dynamicLinkerDepTag:
				depPaths.DynamicLinker = linkFile
			}
		}
	})

	// use the ordered dependencies as this module's dependencies
	orderedStaticPaths, transitiveStaticLibs := orderStaticModuleDeps(directStaticDeps, directSharedDeps)
	depPaths.TranstiveStaticLibrariesForOrdering = transitiveStaticLibs
	depPaths.StaticLibs = append(depPaths.StaticLibs, orderedStaticPaths...)

	// Dedup exported flags from dependencies
	depPaths.Flags = android.FirstUniqueStrings(depPaths.Flags)
	depPaths.IncludeDirs = android.FirstUniquePaths(depPaths.IncludeDirs)
	depPaths.SystemIncludeDirs = android.FirstUniquePaths(depPaths.SystemIncludeDirs)
	depPaths.GeneratedDeps = android.FirstUniquePaths(depPaths.GeneratedDeps)
	depPaths.ReexportedDirs = android.FirstUniquePaths(depPaths.ReexportedDirs)
	depPaths.ReexportedSystemDirs = android.FirstUniquePaths(depPaths.ReexportedSystemDirs)
	depPaths.ReexportedFlags = android.FirstUniqueStrings(depPaths.ReexportedFlags)
	depPaths.ReexportedDeps = android.FirstUniquePaths(depPaths.ReexportedDeps)
	depPaths.ReexportedGeneratedHeaders = android.FirstUniquePaths(depPaths.ReexportedGeneratedHeaders)

	if c.sabi != nil {
		c.sabi.Properties.ReexportedIncludes = android.FirstUniqueStrings(c.sabi.Properties.ReexportedIncludes)
	}

	return depPaths
}

// orderStaticModuleDeps rearranges the order of the static library dependencies of the module
// to match the topological order of the dependency tree, including any static analogues of
// direct shared libraries.  It returns the ordered static dependencies, and an android.DepSet
// of the transitive dependencies.
func orderStaticModuleDeps(staticDeps []StaticLibraryInfo, sharedDeps []SharedLibraryInfo) (ordered android.Paths, transitive *android.DepSet) {
	transitiveStaticLibsBuilder := android.NewDepSetBuilder(android.TOPOLOGICAL)
	var staticPaths android.Paths
	for _, staticDep := range staticDeps {
		staticPaths = append(staticPaths, staticDep.StaticLibrary)
		transitiveStaticLibsBuilder.Transitive(staticDep.TransitiveStaticLibrariesForOrdering)
	}
	for _, sharedDep := range sharedDeps {
		if sharedDep.StaticAnalogue != nil {
			transitiveStaticLibsBuilder.Transitive(sharedDep.StaticAnalogue.TransitiveStaticLibrariesForOrdering)
		}
	}
	transitiveStaticLibs := transitiveStaticLibsBuilder.Build()

	orderedTransitiveStaticLibs := transitiveStaticLibs.ToList()

	// reorder the dependencies based on transitive dependencies
	staticPaths = android.FirstUniquePaths(staticPaths)
	_, orderedStaticPaths := android.FilterPathList(orderedTransitiveStaticLibs, staticPaths)

	if len(orderedStaticPaths) != len(staticPaths) {
		missing, _ := android.FilterPathList(staticPaths, orderedStaticPaths)
		panic(fmt.Errorf("expected %d ordered static paths , got %d, missing %q %q %q", len(staticPaths), len(orderedStaticPaths), missing, orderedStaticPaths, staticPaths))
	}

	return orderedStaticPaths, transitiveStaticLibs
}

// baseLibName trims known prefixes and suffixes
func baseLibName(depName string) string {
	libName := strings.TrimSuffix(depName, llndkLibrarySuffix)
	libName = strings.TrimSuffix(libName, vendorPublicLibrarySuffix)
	libName = strings.TrimPrefix(libName, "prebuilt_")
	return libName
}

func (c *Module) makeLibName(ctx android.ModuleContext, ccDep LinkableInterface, depName string) string {
	vendorSuffixModules := vendorSuffixModules(ctx.Config())
	vendorPublicLibraries := vendorPublicLibraries(ctx.Config())

	libName := baseLibName(depName)
	isLLndk := isLlndkLibrary(libName, ctx.Config())
	isVendorPublicLib := inList(libName, *vendorPublicLibraries)
	bothVendorAndCoreVariantsExist := ccDep.HasVendorVariant() || isLLndk

	if c, ok := ccDep.(*Module); ok {
		// Use base module name for snapshots when exporting to Makefile.
		if c.isSnapshotPrebuilt() {
			baseName := c.BaseModuleName()

			if c.IsVndk() {
				return baseName + ".vendor"
			}

			if vendorSuffixModules[baseName] {
				return baseName + ".vendor"
			} else {
				return baseName
			}
		}
	}

	if ctx.DeviceConfig().VndkUseCoreVariant() && ccDep.IsVndk() && !ccDep.MustUseVendorVariant() &&
		!c.InRamdisk() && !c.InVendorRamdisk() && !c.InRecovery() {
		// The vendor module is a no-vendor-variant VNDK library.  Depend on the
		// core module instead.
		return libName
	} else if c.UseVndk() && bothVendorAndCoreVariantsExist {
		// The vendor module in Make will have been renamed to not conflict with the core
		// module, so update the dependency name here accordingly.
		return libName + c.getNameSuffixWithVndkVersion(ctx)
	} else if (ctx.Platform() || ctx.ProductSpecific()) && isVendorPublicLib {
		return libName + vendorPublicLibrarySuffix
	} else if ccDep.InRamdisk() && !ccDep.OnlyInRamdisk() {
		return libName + ramdiskSuffix
	} else if ccDep.InVendorRamdisk() && !ccDep.OnlyInVendorRamdisk() {
		return libName + vendorRamdiskSuffix
	} else if ccDep.InRecovery() && !ccDep.OnlyInRecovery() {
		return libName + recoverySuffix
	} else if ccDep.Module().Target().NativeBridge == android.NativeBridgeEnabled {
		return libName + nativeBridgeSuffix
	} else {
		return libName
	}
}

func (c *Module) InstallInData() bool {
	if c.installer == nil {
		return false
	}
	return c.installer.inData()
}

func (c *Module) InstallInSanitizerDir() bool {
	if c.installer == nil {
		return false
	}
	if c.sanitize != nil && c.sanitize.inSanitizerDir() {
		return true
	}
	return c.installer.inSanitizerDir()
}

func (c *Module) InstallInRamdisk() bool {
	return c.InRamdisk()
}

func (c *Module) InstallInVendorRamdisk() bool {
	return c.InVendorRamdisk()
}

func (c *Module) InstallInRecovery() bool {
	return c.InRecovery()
}

func (c *Module) MakeUninstallable() {
	if c.installer == nil {
		c.ModuleBase.MakeUninstallable()
		return
	}
	c.installer.makeUninstallable(c)
}

func (c *Module) HostToolPath() android.OptionalPath {
	if c.installer == nil {
		return android.OptionalPath{}
	}
	return c.installer.hostToolPath()
}

func (c *Module) IntermPathForModuleOut() android.OptionalPath {
	return c.outputFile
}

func (c *Module) OutputFiles(tag string) (android.Paths, error) {
	switch tag {
	case "":
		if c.outputFile.Valid() {
			return android.Paths{c.outputFile.Path()}, nil
		}
		return android.Paths{}, nil
	default:
		return nil, fmt.Errorf("unsupported module reference tag %q", tag)
	}
}

func (c *Module) static() bool {
	if static, ok := c.linker.(interface {
		static() bool
	}); ok {
		return static.static()
	}
	return false
}

func (c *Module) staticBinary() bool {
	if static, ok := c.linker.(interface {
		staticBinary() bool
	}); ok {
		return static.staticBinary()
	}
	return false
}

func (c *Module) header() bool {
	if h, ok := c.linker.(interface {
		header() bool
	}); ok {
		return h.header()
	}
	return false
}

func (c *Module) binary() bool {
	if b, ok := c.linker.(interface {
		binary() bool
	}); ok {
		return b.binary()
	}
	return false
}

func (c *Module) object() bool {
	if o, ok := c.linker.(interface {
		object() bool
	}); ok {
		return o.object()
	}
	return false
}

func (c *Module) getMakeLinkType(actx android.ModuleContext) string {
	if c.UseVndk() {
		if lib, ok := c.linker.(*llndkStubDecorator); ok {
			if Bool(lib.Properties.Vendor_available) {
				return "native:vndk"
			}
			return "native:vndk_private"
		}
		if c.IsVndk() && !c.isVndkExt() {
			if Bool(c.VendorProperties.Vendor_available) {
				return "native:vndk"
			}
			return "native:vndk_private"
		}
		if c.inProduct() {
			return "native:product"
		}
		return "native:vendor"
	} else if c.InRamdisk() {
		return "native:ramdisk"
	} else if c.InVendorRamdisk() {
		return "native:vendor_ramdisk"
	} else if c.InRecovery() {
		return "native:recovery"
	} else if c.Target().Os == android.Android && String(c.Properties.Sdk_version) != "" {
		return "native:ndk:none:none"
		// TODO(b/114741097): use the correct ndk stl once build errors have been fixed
		//family, link := getNdkStlFamilyAndLinkType(c)
		//return fmt.Sprintf("native:ndk:%s:%s", family, link)
	} else if actx.DeviceConfig().VndkUseCoreVariant() && !c.MustUseVendorVariant() {
		return "native:platform_vndk"
	} else {
		return "native:platform"
	}
}

// Overrides ApexModule.IsInstallabeToApex()
// Only shared/runtime libraries and "test_per_src" tests are installable to APEX.
func (c *Module) IsInstallableToApex() bool {
	if lib := c.library; lib != nil {
		// Stub libs and prebuilt libs in a versioned SDK are not
		// installable to APEX even though they are shared libs.
		return lib.shared() && !lib.buildStubs() && c.ContainingSdk().Unversioned()
	} else if _, ok := c.linker.(testPerSrc); ok {
		return true
	}
	return false
}

func (c *Module) AvailableFor(what string) bool {
	if linker, ok := c.linker.(interface {
		availableFor(string) bool
	}); ok {
		return c.ApexModuleBase.AvailableFor(what) || linker.availableFor(what)
	} else {
		return c.ApexModuleBase.AvailableFor(what)
	}
}

func (c *Module) TestFor() []string {
	if test, ok := c.linker.(interface {
		testFor() []string
	}); ok {
		return test.testFor()
	} else {
		return c.ApexModuleBase.TestFor()
	}
}

func (c *Module) UniqueApexVariations() bool {
	if u, ok := c.compiler.(interface {
		uniqueApexVariations() bool
	}); ok {
		return u.uniqueApexVariations()
	} else {
		return false
	}
}

// Return true if the module is ever installable.
func (c *Module) EverInstallable() bool {
	return c.installer != nil &&
		// Check to see whether the module is actually ever installable.
		c.installer.everInstallable()
}

func (c *Module) installable(apexInfo android.ApexInfo) bool {
	ret := c.EverInstallable() &&
		// Check to see whether the module has been configured to not be installed.
		proptools.BoolDefault(c.Properties.Installable, true) &&
		!c.Properties.PreventInstall && c.outputFile.Valid()

	// The platform variant doesn't need further condition. Apex variants however might not
	// be installable because it will likely to be included in the APEX and won't appear
	// in the system partition.
	if apexInfo.IsForPlatform() {
		return ret
	}

	// Special case for modules that are configured to be installed to /data, which includes
	// test modules. For these modules, both APEX and non-APEX variants are considered as
	// installable. This is because even the APEX variants won't be included in the APEX, but
	// will anyway be installed to /data/*.
	// See b/146995717
	if c.InstallInData() {
		return ret
	}

	return false
}

func (c *Module) AndroidMkWriteAdditionalDependenciesForSourceAbiDiff(w io.Writer) {
	if c.linker != nil {
		if library, ok := c.linker.(*libraryDecorator); ok {
			library.androidMkWriteAdditionalDependenciesForSourceAbiDiff(w)
		}
	}
}

func (c *Module) DepIsInSameApex(ctx android.BaseModuleContext, dep android.Module) bool {
	depTag := ctx.OtherModuleDependencyTag(dep)
	libDepTag, isLibDepTag := depTag.(libraryDependencyTag)

	if cc, ok := dep.(*Module); ok {
		if cc.HasStubsVariants() {
			if isLibDepTag && libDepTag.shared() {
				// dynamic dep to a stubs lib crosses APEX boundary
				return false
			}
			if IsRuntimeDepTag(depTag) {
				// runtime dep to a stubs lib also crosses APEX boundary
				return false
			}
		}
		if isLibDepTag && c.static() && libDepTag.shared() {
			// shared_lib dependency from a static lib is considered as crossing
			// the APEX boundary because the dependency doesn't actually is
			// linked; the dependency is used only during the compilation phase.
			return false
		}
	}
	if depTag == stubImplDepTag || depTag == llndkImplDep {
		// We don't track beyond LLNDK or from an implementation library to its stubs.
		return false
	}
	return true
}

func (c *Module) ShouldSupportSdkVersion(ctx android.BaseModuleContext,
	sdkVersion android.ApiLevel) error {
	// We ignore libclang_rt.* prebuilt libs since they declare sdk_version: 14(b/121358700)
	if strings.HasPrefix(ctx.OtherModuleName(c), "libclang_rt") {
		return nil
	}
	// b/154569636: set min_sdk_version correctly for toolchain_libraries
	if c.ToolchainLibrary() {
		return nil
	}
	// We don't check for prebuilt modules
	if _, ok := c.linker.(prebuiltLinkerInterface); ok {
		return nil
	}
	minSdkVersion := c.MinSdkVersion()
	if minSdkVersion == "apex_inherit" {
		return nil
	}
	if minSdkVersion == "" {
		// JNI libs within APK-in-APEX fall into here
		// Those are okay to set sdk_version instead
		// We don't have to check if this is a SDK variant because
		// non-SDK variant resets sdk_version, which works too.
		minSdkVersion = c.SdkVersion()
	}
	if minSdkVersion == "" {
		return fmt.Errorf("neither min_sdk_version nor sdk_version specificed")
	}
	// Not using nativeApiLevelFromUser because the context here is not
	// necessarily a native context.
	ver, err := android.ApiLevelFromUser(ctx, minSdkVersion)
	if err != nil {
		return err
	}

	if ver.GreaterThan(sdkVersion) {
		return fmt.Errorf("newer SDK(%v)", ver)
	}
	return nil
}

//
// Defaults
//
type Defaults struct {
	android.ModuleBase
	android.DefaultsModuleBase
	android.ApexModuleBase
}

// cc_defaults provides a set of properties that can be inherited by other cc
// modules. A module can use the properties from a cc_defaults using
// `defaults: ["<:default_module_name>"]`. Properties of both modules are
// merged (when possible) by prepending the default module's values to the
// depending module's values.
func defaultsFactory() android.Module {
	return DefaultsFactory()
}

func DefaultsFactory(props ...interface{}) android.Module {
	module := &Defaults{}

	module.AddProperties(props...)
	module.AddProperties(
		&BaseProperties{},
		&VendorProperties{},
		&BaseCompilerProperties{},
		&BaseLinkerProperties{},
		&ObjectLinkerProperties{},
		&LibraryProperties{},
		&StaticProperties{},
		&SharedProperties{},
		&FlagExporterProperties{},
		&BinaryLinkerProperties{},
		&TestProperties{},
		&TestBinaryProperties{},
		&BenchmarkProperties{},
		&FuzzProperties{},
		&StlProperties{},
		&SanitizeProperties{},
		&StripProperties{},
		&InstallerProperties{},
		&TidyProperties{},
		&CoverageProperties{},
		&SAbiProperties{},
		&VndkProperties{},
		&LTOProperties{},
		&PgoProperties{},
		&android.ProtoProperties{},
		// RustBindgenProperties is included here so that cc_defaults can be used for rust_bindgen modules.
		&RustBindgenClangProperties{},
	)

	android.InitDefaultsModule(module)

	return module
}

func squashVendorSrcs(m *Module) {
	if lib, ok := m.compiler.(*libraryDecorator); ok {
		lib.baseCompiler.Properties.Srcs = append(lib.baseCompiler.Properties.Srcs,
			lib.baseCompiler.Properties.Target.Vendor.Srcs...)

		lib.baseCompiler.Properties.Exclude_srcs = append(lib.baseCompiler.Properties.Exclude_srcs,
			lib.baseCompiler.Properties.Target.Vendor.Exclude_srcs...)

		lib.baseCompiler.Properties.Exclude_generated_sources = append(lib.baseCompiler.Properties.Exclude_generated_sources,
			lib.baseCompiler.Properties.Target.Vendor.Exclude_generated_sources...)
	}
}

func squashRecoverySrcs(m *Module) {
	if lib, ok := m.compiler.(*libraryDecorator); ok {
		lib.baseCompiler.Properties.Srcs = append(lib.baseCompiler.Properties.Srcs,
			lib.baseCompiler.Properties.Target.Recovery.Srcs...)

		lib.baseCompiler.Properties.Exclude_srcs = append(lib.baseCompiler.Properties.Exclude_srcs,
			lib.baseCompiler.Properties.Target.Recovery.Exclude_srcs...)

		lib.baseCompiler.Properties.Exclude_generated_sources = append(lib.baseCompiler.Properties.Exclude_generated_sources,
			lib.baseCompiler.Properties.Target.Recovery.Exclude_generated_sources...)
	}
}

func squashVendorRamdiskSrcs(m *Module) {
	if lib, ok := m.compiler.(*libraryDecorator); ok {
		lib.baseCompiler.Properties.Exclude_srcs = append(lib.baseCompiler.Properties.Exclude_srcs, lib.baseCompiler.Properties.Target.Vendor_ramdisk.Exclude_srcs...)
	}
}

func (c *Module) IsSdkVariant() bool {
	return c.Properties.IsSdkVariant || c.AlwaysSdk()
}

func kytheExtractAllFactory() android.Singleton {
	return &kytheExtractAllSingleton{}
}

type kytheExtractAllSingleton struct {
}

func (ks *kytheExtractAllSingleton) GenerateBuildActions(ctx android.SingletonContext) {
	var xrefTargets android.Paths
	ctx.VisitAllModules(func(module android.Module) {
		if ccModule, ok := module.(xref); ok {
			xrefTargets = append(xrefTargets, ccModule.XrefCcFiles()...)
		}
	})
	// TODO(asmundak): Perhaps emit a rule to output a warning if there were no xrefTargets
	if len(xrefTargets) > 0 {
		ctx.Phony("xref_cxx", xrefTargets...)
	}
}

var Bool = proptools.Bool
var BoolDefault = proptools.BoolDefault
var BoolPtr = proptools.BoolPtr
var String = proptools.String
var StringPtr = proptools.StringPtr
