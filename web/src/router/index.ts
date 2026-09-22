import { createRouter, createWebHashHistory } from 'vue-router'

// 用 hash 路由：后端把「能对应到嵌入文件的路径」返回静态资源、其余路径转发给上游，
// hash 不会出现在请求路径里，因此管理界面与代理入口可以干净地共用一个端口。
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: '/dashboard' },
    {
      path: '/dashboard',
      name: 'dashboard',
      component: () => import('@/views/Dashboard.vue'),
      meta: { title: '仪表盘' },
    },
    {
      path: '/providers',
      name: 'providers',
      component: () => import('@/views/Providers.vue'),
      meta: { title: '供应商' },
    },
    {
      path: '/logs',
      name: 'logs',
      component: () => import('@/views/Logs.vue'),
      meta: { title: '请求日志' },
    },
    {
      path: '/live',
      name: 'live',
      component: () => import('@/views/Live.vue'),
      meta: { title: '实时监控' },
    },
    {
      path: '/settings',
      name: 'settings',
      component: () => import('@/views/Settings.vue'),
      meta: { title: '设置' },
    },
    { path: '/:pathMatch(.*)*', redirect: '/dashboard' },
  ],
})

export default router
