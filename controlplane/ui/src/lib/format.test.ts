import { describe, expect, it } from 'vitest'
import { formatMetric, headline, resourceLabel, shortId } from './format'

describe('headline', () => {
  it('keeps the first sentence without its full stop', () => {
    expect(headline('Split the fields. Then code each.')).toBe('Split the fields')
  })

  it('cuts a long first sentence at a word', () => {
    const label = headline('word '.repeat(40), 20)
    expect(label).toBe('word word word word…')
  })

  it('leaves a short text whole', () => {
    expect(headline('Try xor-delta on 1.5x data')).toBe('Try xor-delta on 1.5x data')
  })
})

describe('shortId', () => {
  it('keeps the kind and the end that tells ids apart', () => {
    expect(shortId('job-01a16f7c143470bfa90b638abf04f2da')).toBe('job-…04f2da')
    expect(shortId('01a16f7b-9d52-7271-9be5-4fd09c30d082')).toBe('…30d082')
  })

  it('leaves a short id whole', () => {
    expect(shortId('ag-1234')).toBe('ag-1234')
  })
})

describe('formatMetric', () => {
  it('shows four significant digits', () => {
    expect(formatMetric(0.37109421208296317)).toBe('0.3711')
    expect(formatMetric(0.875)).toBe('0.8750')
    expect(formatMetric(12.3456)).toBe('12.35')
    expect(formatMetric(123456.7)).toBe('123,457')
    expect(formatMetric(0.0000123)).toBe('1.23e-5')
    expect(formatMetric(0)).toBe('0')
  })
})

describe('headline of a labelled claim', () => {
  it('drops the label', () => {
    expect(headline('Mechanism: tanh is bounded. Idea: try it.')).toBe('tanh is bounded')
  })
})

describe('the name of a priced type', () => {
  it('drops the selector key and the vendor prefix', () => {
    expect(resourceLabel('nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe')).toBe('A100 80GB PCIe')
    expect(resourceLabel('nvidia.com/gpu.product=NVIDIA-GeForce-RTX-4090')).toBe('RTX 4090')
    expect(resourceLabel('hypothesisloop.dev/cpu=core')).toBe('CPU core')
    expect(resourceLabel('tenstorrent.com/chipArch=blackhole')).toBe('Tenstorrent Blackhole')
    expect(resourceLabel('nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3')).toBe('H100 80GB HBM3')
    expect(resourceLabel('nvidia.com/gpu.product=NVIDIA-A100-SXM4-80GB')).toBe('A100 SXM4 80GB')
    expect(resourceLabel('nvidia.com/gpu.product=NVIDIA-GeForce-RTX-5090')).toBe('RTX 5090')
    expect(resourceLabel('nvidia.com/gpu.product=NVIDIA-RTX-A6000')).toBe('RTX A6000')
    expect(resourceLabel('amd.com/gpu.product-name=AMD_Instinct_MI300X')).toBe('AMD Instinct MI300X')
  })
})
