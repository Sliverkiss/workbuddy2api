import { createApp } from 'vue';
import { createPinia } from 'pinia';
import App from './App.vue';
import { router } from './router';
import { applyTheme, getStoredTheme } from './lib/theme';
import './style.css';

applyTheme(getStoredTheme());

createApp(App).use(createPinia()).use(router).mount('#app');
