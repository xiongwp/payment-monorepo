import axios, { AxiosInstance, AxiosRequestConfig, AxiosResponse } from 'axios';
import type { ApiResponse } from '../types/accounting';

const client: AxiosInstance = axios.create({
  baseURL: '/api',           // vite proxy: /api → accounting-service:9090
  timeout: 30_000,
  headers: { 'Content-Type': 'application/json' },
});

// 请求拦截：可在此注入 token
client.interceptors.request.use((config) => {
  const token = localStorage.getItem('token');
  if (token) config.headers.Authorization = `Bearer ${token}`;
  return config;
});

// 响应拦截：统一错误处理
client.interceptors.response.use(
  (res: AxiosResponse<ApiResponse>) => {
    if (res.data.code !== 0) {
      return Promise.reject(new Error(res.data.message || 'API error'));
    }
    return res;
  },
  (err) => {
    const msg =
      err.response?.data?.message ||
      err.message ||
      'Network error';
    return Promise.reject(new Error(msg));
  },
);

export async function request<T>(config: AxiosRequestConfig): Promise<T> {
  const res = await client.request<ApiResponse<T>>(config);
  return res.data.data as T;
}

export default client;
