<template>
  <span
    v-if="displayVersion"
    class="rounded-lg bg-gray-100 px-2 py-1 text-xs font-medium text-gray-600 dark:bg-dark-800 dark:text-dark-400"
    :title="t('version.currentVersion')"
  >
    v{{ displayVersion }}
  </span>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAuthStore, useAppStore } from '@/stores'

const { t } = useI18n()

const props = defineProps<{
  version?: string
}>()

const authStore = useAuthStore()
const appStore = useAppStore()

const displayVersion = computed(() => appStore.currentVersion || props.version || '')

onMounted(() => {
  if (authStore.isAdmin) {
    void appStore.fetchVersion()
  }
})
</script>
