import type { UsageLog } from '@/types'

type ImageOutputTokenRow = Pick<UsageLog, 'output_tokens' | 'image_output_tokens'>
type ImageOutputCostRow = Pick<UsageLog, 'image_output_cost'>
type ImageInputTokenRow = Pick<UsageLog, 'input_tokens' | 'image_input_tokens'>
type ImageInputCostRow = Pick<UsageLog, 'image_input_cost'>

export const hasImageOutputTokens = (row: ImageOutputTokenRow | null | undefined): boolean =>
  (row?.image_output_tokens ?? 0) > 0

export const textOutputTokens = (row: ImageOutputTokenRow | null | undefined): number =>
  Math.max(0, (row?.output_tokens ?? 0) - (row?.image_output_tokens ?? 0))

export const hasImageOutputCost = (row: ImageOutputCostRow | null | undefined): boolean =>
  (row?.image_output_cost ?? 0) > 0

export const hasImageInputTokens = (row: ImageInputTokenRow | null | undefined): boolean =>
  (row?.image_input_tokens ?? 0) > 0

export const textInputTokens = (row: ImageInputTokenRow | null | undefined): number =>
  Math.max(0, (row?.input_tokens ?? 0) - (row?.image_input_tokens ?? 0))

export const hasImageInputCost = (row: ImageInputCostRow | null | undefined): boolean =>
  (row?.image_input_cost ?? 0) > 0
