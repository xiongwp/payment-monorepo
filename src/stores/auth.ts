// 风控 admin token + role state. zustand store 让任何组件读取当前 role
// (RBAC UI 隐藏 danger-level 按钮的地方)。
import { create } from 'zustand'
import { whoami } from '../api/risk'
import type { AdminRole } from '../api/risk'

interface AuthState {
  token: string                  // 当前 localStorage token (空=dev)
  role: AdminRole                // 服务端 whoami 返回；空=未鉴权
  keyID: string                  // sha256 prefix；给 audit log 看
  loaded: boolean

  setToken: (t: string) => void
  refresh: () => Promise<void>
  hasRole: (need: AdminRole) => boolean
}

export const useAuth = create<AuthState>((set, get) => ({
  token: localStorage.getItem('admin_token') || '',
  role: '',
  keyID: '',
  loaded: false,

  setToken: (t) => {
    if (t) {
      localStorage.setItem('admin_token', t)
    } else {
      localStorage.removeItem('admin_token')
    }
    set({ token: t })
    // 异步刷新 role
    void get().refresh()
  },

  refresh: async () => {
    try {
      const r = await whoami()
      set({ role: r.role, keyID: r.key_id, loaded: true })
    } catch {
      set({ role: '', keyID: '', loaded: true })
    }
  },

  // role 蕴含跟 risk-manage 后端 HasRole 一致：
  // danger > write > read。空 role (dev) 视作 danger (老 AdminAuth 兼容)。
  hasRole: (need) => {
    const r = get().role
    if (!r) return true                       // dev / 老 token 默认 danger
    if (need === 'read') return r === 'read' || r === 'write' || r === 'danger'
    if (need === 'write') return r === 'write' || r === 'danger'
    if (need === 'danger') return r === 'danger'
    return true
  },
}))
