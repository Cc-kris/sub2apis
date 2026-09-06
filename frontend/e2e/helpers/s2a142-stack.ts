import { execFileSync } from 'node:child_process'
import path from 'node:path'

const repoRoot = path.resolve(process.cwd(), process.cwd().endsWith('/frontend') ? '..' : '.')
const stackScript = path.join(repoRoot, 'backend/scripts/s2a142-e2e-stack.sh')

export function stackCommand(command: 'up' | 'seed' | 'health' | 'cleanup' | 'down', runId: string): string {
  return execFileSync(stackScript, [command, runId], { cwd: repoRoot, encoding: 'utf8' })
}

export function stackExports(runId: string) {
  const output = stackCommand('seed', runId)
  const values = Object.fromEntries(
    output.trim().split('\n').map((line) => {
      const match = line.match(/^export ([A-Z_]+)='?(.*?)'?$/)
      return match ? [match[1], match[2]] : ['', '']
    }).filter(([key]) => key)
  )
  return {
    baseURL: values.E2E_BASE_URL,
    adminEmail: values.E2E_ADMIN_EMAIL,
    adminPassword: values.E2E_ADMIN_PASSWORD
  }
}
