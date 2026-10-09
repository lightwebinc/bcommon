/**
 * What an application's bundle step checks in esbuild's metafile before it
 * keeps a host module's bundle, apart from the build so that a test can feed
 * it metafiles no clean build produces.
 */

/**
 * The part of esbuild's metafile the check reads. esbuild's own Metafile
 * satisfies it, and a test's synthetic one need carry nothing more.
 */
export interface CheckedMetafile {
  inputs: Record<string, unknown>
  outputs: Record<string, { imports: Array<{ path: string }> }>
}

/** What a bundle may import besides sdk. */
export interface BundleRules {
  /** Node's built-in modules (`node:...`); default true. A bundle for a host that gives it none says false. */
  nodeBuiltins?: boolean
}

/**
 * Whether path is one of root's own files. A node_modules/ below root holds
 * another package: npm nests a dependency there when its version conflicts
 * with the hoisted copy, so the prefix alone would let that package's code
 * through under the library's name.
 */
function ownFile(path: string, root: string): boolean {
  return path.startsWith(root) && !path.slice(root.length).includes('node_modules/')
}

/**
 * The reasons to refuse the bundle, empty when it may be kept: an input that
 * is not a file of src/ or of the library's own package, no input of the
 * library's at all, or an import in outfile other than sdk (or, unless the
 * rules say otherwise, a node: built-in). Each reason is one line naming
 * what failed.
 */
export function bundleRefusals(metafile: CheckedMetafile, outfile: string, library: string, sdk: string, rules: BundleRules = {}): string[] {
  const builtins = rules.nodeBuiltins ?? true
  const roots = ['src/', `node_modules/${library}/`]
  const inputs = Object.keys(metafile.inputs)
  const problems: string[] = []
  for (const path of inputs) {
    if (roots.some((root) => ownFile(path, root))) continue
    const nestedIn = roots.find((root) => path.startsWith(root))
    problems.push(nestedIn === undefined ? `input outside ${roots.join(' and ')}: ${path}` : `input from a package nested under ${nestedIn}: ${path}`)
  }
  // An empty answer is not a clean one: a library marked external, or
  // resolved from somewhere else, would pass the check above and leave the
  // host to find a package it does not have.
  if (!inputs.some((path) => ownFile(path, roots[1]!))) problems.push(`nothing from ${library} was inlined`)
  const output = metafile.outputs[outfile]
  if (output === undefined) {
    problems.push(`esbuild reported no ${outfile}`)
  } else {
    for (const path of new Set(output.imports.map((imp) => imp.path))) {
      if (path === sdk || (builtins && path.startsWith('node:'))) continue
      problems.push(builtins ? `import other than ${sdk} or a node: built-in: ${path}` : `import other than ${sdk}: ${path}`)
    }
  }
  return problems
}
