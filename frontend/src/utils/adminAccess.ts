const opsRoles = new Set(['ops', 'operator', 'operation', 'operations'])
const businessRoles = new Set(['business', 'business_operator', 'business-operator', 'yunying', '运营'])
const supportRoles = new Set(['customer_service', 'customer-service', 'customerservice', 'support'])

export function normalizeAdminViewerRole(role: unknown): string {
  return String(role ?? '').trim().toLowerCase()
}

export function isPlatformOwnerRole(role: unknown): boolean {
  const normalized = normalizeAdminViewerRole(role)
  return normalized === '' || normalized === 'admin'
}

export function isOpsRole(role: unknown): boolean {
  return opsRoles.has(normalizeAdminViewerRole(role))
}

export function isBusinessRole(role: unknown): boolean {
  return businessRoles.has(normalizeAdminViewerRole(role))
}

export function isSupportRole(role: unknown): boolean {
  return supportRoles.has(normalizeAdminViewerRole(role))
}

export function hasScopedAdminAccess(role: unknown): boolean {
  return isPlatformOwnerRole(role) || isOpsRole(role) || isBusinessRole(role) || isSupportRole(role)
}

export function canAccessOpsOverview(role: unknown): boolean {
  return isPlatformOwnerRole(role) || isOpsRole(role) || isBusinessRole(role) || isSupportRole(role)
}

export function canAccessOpsAlertRules(role: unknown): boolean {
  return isPlatformOwnerRole(role) || isOpsRole(role)
}

export function canAccessOpsAIAnalysis(role: unknown): boolean {
  return isPlatformOwnerRole(role) || isOpsRole(role)
}

export function canAccessAdminPath(path: string, role: unknown): boolean {
  if (isPlatformOwnerRole(role)) {
    return true
  }
  if (/^\/admin\/ops\/errors\/\d+$/.test(path)) {
    return canAccessOpsOverview(role)
  }

  switch (path) {
    case '/admin':
    case '/admin/ops':
    case '/admin/ops/overview':
    case '/admin/ops/errors':
      return canAccessOpsOverview(role)
    case '/admin/ops/alert-rules':
      return canAccessOpsAlertRules(role)
    case '/admin/ops/ai-analysis':
      return canAccessOpsAIAnalysis(role)
    default:
      return false
  }
}

export function resolveAdminHomePath(role: unknown): string {
  if (isPlatformOwnerRole(role)) {
    return '/admin/dashboard'
  }
  if (canAccessOpsOverview(role)) {
    return '/admin/ops/overview'
  }
  return '/dashboard'
}
