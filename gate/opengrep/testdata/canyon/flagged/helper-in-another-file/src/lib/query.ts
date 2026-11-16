export function searchStatement(term: string): string {
  return "SELECT * FROM documents WHERE title LIKE '%" + term + "%'";
}
