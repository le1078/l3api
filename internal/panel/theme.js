'use strict';
/* 首帧主题引导（FOUC 修复，2026-10-13）。
 *
 * 必须在 <head> 里**同步**加载、且在页面绘制前执行：否则 <html data-theme>
 * 会先落到深色默认值，等 body 末尾的 app.js 读到浅色偏好才翻转 —— 用户看到
 * 的「深色闪一下再变白」就是这么来的。
 *
 * 与 app.js 的 theme 逻辑保持一致：theme ∈ {auto,light,dark}，auto 跟随系统；
 * 键名与 localStorage 值域必须完全一致（LS_THEME = 'l3api.theme'）。
 * 本脚本只负责「定初始值」，不做图标/事件绑定——那些仍由 app.js 接管。
 *
 * 独立文件而非内联 <script>：CSP 为 script-src 'self'（禁 unsafe-inline）。 */
(function () {
  var t;
  try { t = localStorage.getItem('l3api.theme') || 'auto'; } catch (e) { t = 'auto'; }
  var eff = t === 'auto'
    ? (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark')
    : t;
  document.documentElement.dataset.theme = eff;
})();
