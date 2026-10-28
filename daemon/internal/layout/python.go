package layout

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bearbot/daemon/internal/proc"
)

// Time limits of the Python helper commands.
const (
	VenvTimeout          = 10 * time.Minute
	PythonVersionTimeout = time.Minute
	PipInstallTimeout    = time.Hour
)

// RebuildCommand is printed when the virtual environment does not match the image's interpreter (DESIGN 4.7).
const RebuildCommand = "docker run --rm <挂载参数> bearbot python rebuild"

// PythonMismatchError means the virtual environment cannot be used with the image's interpreter. Its message holds
// the reason and the rebuild command.
type PythonMismatchError struct {
	Reason string
}

func (e *PythonMismatchError) Error() string {
	return e.Reason + "\n请执行以下命令重建 Python 虚拟环境：\n  " + RebuildCommand
}

// RunSpec returns the description of a helper command that runs as who (without Credential in rootless mode), for
// proc.Run and proc.RunChecked.
func (u *Users) RunSpec(who User, path string, args, env []string, dir string, timeout time.Duration) proc.RunSpec {
	return proc.RunSpec{Path: path, Args: args, Env: env, Dir: dir, Credential: u.Credential(who), Timeout: timeout}
}

func (p Paths) asBearbot(u *Users, path string, args []string, env []string, timeout time.Duration) proc.RunSpec {
	return u.RunSpec(u.Bearbot, path, args, env, p.BearbotHome, timeout)
}

func ensureVenv(ctx context.Context, p Paths, u *Users) (bool, error) {
	info, err := os.Lstat(p.PythonVenv)
	create := false
	switch {
	case errors.Is(err, os.ErrNotExist):
		create = true
	case err != nil:
		return false, fmt.Errorf("无法访问 %s：%w", p.PythonVenv, err)
	case !info.IsDir():
		return false, fmt.Errorf("%s 应当是目录，实际是 %s", p.PythonVenv, describeMode(info.Mode()))
	default:
		empty, err := dirEmpty(p.PythonVenv)
		if err != nil {
			return false, err
		}
		create = empty
	}
	if create {
		if err := CreateVenv(ctx, p, u); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := EnsureDir(p.VenvDir(), u); err != nil {
		return false, err
	}
	return false, nil
}

// CreateVenv creates the virtual environment directory (owner bearbot, 0775) when it is missing and runs
// `uv venv --seed --python <interpreter> <PythonVenv>` as bearbot (DESIGN 4.7). The directory must be missing or
// empty. The interpreter is the resolved target of Python: uv records the path it is given, and a venv bound to
// the symlink /usr/local/bin/python3.14 would silently run whatever interpreter a newer image puts behind it, so
// the version check could never see the change.
func CreateVenv(ctx context.Context, p Paths, u *Users) error {
	if _, err := EnsureDir(p.VenvDir(), u); err != nil {
		return err
	}
	interpreter, err := filepath.EvalSymlinks(p.Python)
	if err != nil {
		return fmt.Errorf("找不到镜像中的 Python 解释器 %s：%w", p.Python, err)
	}
	env := append(p.ToolEnv(), "UV_LINK_MODE=copy")
	spec := p.asBearbot(u, p.Uv, []string{"venv", "--seed", "--python", interpreter, p.PythonVenv}, env, VenvTimeout)
	if _, err := proc.RunChecked(ctx, spec); err != nil {
		return fmt.Errorf("创建 Python 虚拟环境 %s 失败：%w", p.PythonVenv, err)
	}
	return nil
}

func pythonVersion(ctx context.Context, p Paths, u *Users, python string) (string, error) {
	spec := p.asBearbot(u, python, []string{"-c", "import sys; print(sys.version)"}, p.ToolEnv(), PythonVersionTimeout)
	res, err := proc.RunChecked(ctx, spec)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// CheckPython compares sys.version of the virtual environment's interpreter with the image's interpreter, both run
// as bearbot (DESIGN 4.7). A failure or a difference returns *PythonMismatchError. It returns the version.
func CheckPython(ctx context.Context, p Paths, u *Users) (string, error) {
	image, err := pythonVersion(ctx, p, u, p.Python)
	if err != nil {
		return "", &PythonMismatchError{Reason: fmt.Sprintf("无法执行镜像中的 Python 解释器 %s：%v", p.Python, err)}
	}
	venv, err := pythonVersion(ctx, p, u, p.PythonBin)
	if err != nil {
		return "", &PythonMismatchError{Reason: fmt.Sprintf("无法执行虚拟环境 %s 中的 Python 解释器（镜像可能更换了 Python 版本）：%v", p.PythonVenv, err)}
	}
	if venv != image {
		return "", &PythonMismatchError{Reason: fmt.Sprintf("虚拟环境 %s 的 Python 版本与镜像中的不同：虚拟环境为 %s，镜像为 %s", p.PythonVenv, venv, image)}
	}
	return venv, nil
}

// listPackagesScript prints the [name, version] pairs of the distributions in the directory given as argv[1],
// without pip, as JSON.
const listPackagesScript = `
import importlib.metadata, json, re, sys
found = {}
for dist in importlib.metadata.distributions(path=[sys.argv[1]]):
    name = dist.metadata["Name"]
    if not name:
        sys.exit("distribution without a name: %s" % dist._path)
    key = re.sub(r"[-_.]+", "-", name).lower()
    if key == "pip":
        continue
    found[key] = [name, dist.version]
print(json.dumps(sorted(found.values())))
`

// Package is an installed Python distribution.
type Package struct {
	Name    string
	Version string
}

// venvVersion reads version_info (uv) or version (python -m venv) from pyvenv.cfg.
func venvVersion(venv string) (string, error) {
	f, err := os.Open(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		return "", fmt.Errorf("无法读取 %s：%w", filepath.Join(venv, "pyvenv.cfg"), err)
	}
	defer f.Close()
	values := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			values[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	for _, k := range []string{"version_info", "version"} {
		if v := values[k]; v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s 中没有 version_info 或 version", filepath.Join(venv, "pyvenv.cfg"))
}

// PythonRebuild rebuilds the virtual environment (DESIGN 4.7, `bearbotd python rebuild`): list the packages of the
// old environment with the image's interpreter, rename the old environment to python.<old version>.old, create a
// new environment, and install the same versions with `uv pip install`. Progress is written to out. On failure the
// old environment stays at its new name.
func PythonRebuild(ctx context.Context, p Paths, u *Users, out io.Writer) error {
	info, err := os.Lstat(p.PythonVenv)
	if err != nil {
		return fmt.Errorf("无法访问虚拟环境 %s：%w", p.PythonVenv, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 应当是目录，实际是 %s", p.PythonVenv, describeMode(info.Mode()))
	}
	oldVersion, err := venvVersion(p.PythonVenv)
	if err != nil {
		return err
	}
	sites, err := filepath.Glob(filepath.Join(p.PythonVenv, "lib", "python*", "site-packages"))
	if err != nil {
		return err
	}
	if len(sites) != 1 {
		return fmt.Errorf("在 %s 中找到 %d 个 site-packages 目录，应当正好有一个：%v", p.PythonVenv, len(sites), sites)
	}
	fmt.Fprintf(out, "旧虚拟环境：%s（Python %s）\n", p.PythonVenv, oldVersion)

	spec := p.asBearbot(u, p.Python, []string{"-c", listPackagesScript, sites[0]}, p.ToolEnv(), PythonVersionTimeout)
	res, err := proc.RunChecked(ctx, spec)
	if err != nil {
		return fmt.Errorf("列出旧虚拟环境中的包失败：%w", err)
	}
	var pairs [][2]string
	if err := json.Unmarshal(res.Stdout, &pairs); err != nil {
		return fmt.Errorf("无法解析包列表 %q：%w", res.Stdout, err)
	}
	packages := make([]Package, 0, len(pairs))
	for _, pr := range pairs {
		packages = append(packages, Package{Name: pr[0], Version: pr[1]})
	}
	fmt.Fprintf(out, "旧虚拟环境中有 %d 个包（不含 pip）\n", len(packages))
	for _, pkg := range packages {
		fmt.Fprintf(out, "  %s==%s\n", pkg.Name, pkg.Version)
	}

	backup := filepath.Join(p.ProfileDir, "python."+oldVersion+".old")
	if _, err := os.Lstat(backup); err == nil {
		return fmt.Errorf("%s 已经存在，请先移走它", backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("无法访问 %s：%w", backup, err)
	}
	if err := os.Rename(p.PythonVenv, backup); err != nil {
		return fmt.Errorf("把 %s 改名为 %s 失败：%w", p.PythonVenv, backup, err)
	}
	fmt.Fprintf(out, "旧虚拟环境已改名为 %s\n", backup)

	if err := CreateVenv(ctx, p, u); err != nil {
		return err
	}
	version, err := pythonVersion(ctx, p, u, p.PythonBin)
	if err != nil {
		return fmt.Errorf("新虚拟环境的解释器无法执行：%w", err)
	}
	fmt.Fprintf(out, "已创建新虚拟环境：%s（Python %s）\n", p.PythonVenv, version)

	if len(packages) > 0 {
		args := []string{"pip", "install"}
		for _, pkg := range packages {
			args = append(args, pkg.Name+"=="+pkg.Version)
		}
		env := append(p.ToolEnv(), "VIRTUAL_ENV="+p.PythonVenv, "UV_LINK_MODE=copy")
		res, err := proc.RunChecked(ctx, p.asBearbot(u, p.Uv, args, env, PipInstallTimeout))
		if err != nil {
			return fmt.Errorf("安装旧虚拟环境中的包失败：%w", err)
		}
		out.Write(res.Output)
	}
	fmt.Fprintln(out, "Python 虚拟环境重建完成")
	return nil
}
