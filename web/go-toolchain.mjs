import {accessSync, constants, realpathSync, statSync} from 'node:fs'
import {delimiter, dirname, isAbsolute, join} from 'node:path'

/** Resolve once and execute an absolute binary; never let child_process search PATH. */
export function resolveGoBinary(env = process.env) {
    if (env.PI_GO_BINARY) return trustedExecutable(env.PI_GO_BINARY)
    if (env.GOROOT) return trustedExecutable(join(env.GOROOT, 'bin', executableName()))
    const directories = (env.PATH ?? '').split(delimiter).filter(directory => isAbsolute(directory))
    for (const directory of directories) {
        try { return trustedExecutable(join(directory, executableName())) }
        catch { /* Ignore missing executables and unsafe search directories. */ }
    }
    throw new Error('No trusted Go compiler found. Set PI_GO_BINARY to its absolute path.')
}

function executableName() {
    return process.platform === 'win32' ? 'go.exe' : 'go'
}

function trustedExecutable(path) {
    if (!isAbsolute(path)) throw new Error('PI_GO_BINARY/GOROOT must specify an absolute compiler path')
    const binary = realpathSync(path)
    const file = statSync(binary)
    if (!file.isFile()) throw new Error('Go compiler must be a regular file')
    if (process.platform !== 'win32') {
        assertTrustedOwner(file)
        if (file.mode & 0o022) throw new Error('Go compiler must not be group/world writable')
        assertTrustedAncestors(dirname(binary))
    }
    accessSync(binary, constants.X_OK)
    return binary
}

function assertTrustedOwner(stat) {
    const uid = process.geteuid?.() ?? process.getuid?.()
    if (stat.uid !== 0 && stat.uid !== uid) throw new Error('Go compiler path must be owned by root or the current user')
}

function assertTrustedAncestors(directory) {
    for (;;) {
        const stat = statSync(directory)
        if (!stat.isDirectory()) throw new Error('Go compiler ancestor must be a directory')
        assertTrustedOwner(stat)
        // A trusted-owner sticky directory (e.g. /tmp) cannot unlink/rename its
        // already-validated, trusted-owner child. Non-sticky writable ancestors can.
        if ((stat.mode & 0o022) && !(stat.mode & 0o1000)) {
            throw new Error('Go compiler ancestors must not be group/world writable without sticky protection')
        }
        const parent = dirname(directory)
        if (parent === directory) return
        directory = parent
    }
}
