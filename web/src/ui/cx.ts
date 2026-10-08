/** Joins class names, skipping empty ones (CSS module lookups are `string | undefined`). */
export function cx(...names: (string | false | null | undefined)[]): string {
  return names.filter((n): n is string => typeof n === 'string' && n !== '').join(' ');
}
