import axios, { AxiosRequestConfig, AxiosResponse } from 'axios'

export interface ApiResponse<T = unknown> {
  code: number
  message: string
  data?: T
}

const client = axios.create({
  baseURL: '/api',
  timeout: 30_000,
  headers: { 'Content-Type': 'application/json' },
})

// 请求拦截：注入 bearer token（如果 localStorage 里有）
client.interceptors.request.use((config) => {
  const token = localStorage.getItem('admin_token')
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

// 响应拦截：统一错误 shape
client.interceptors.response.use(
  (res: AxiosResponse<ApiResponse>) => {
    if (res.data.code !== 0) {
      return Promise.reject(new Error(res.data.message || 'API error'))
    }
    return res
  },
  (err) => {
    const msg = err.response?.data?.message || err.message || 'Network error'
    return Promise.reject(new Error(msg))
  },
)

export async function request<T>(config: AxiosRequestConfig): Promise<T> {
  const res = await client.request<ApiResponse<T>>(config)
  return res.data.data as T
}

export default client
