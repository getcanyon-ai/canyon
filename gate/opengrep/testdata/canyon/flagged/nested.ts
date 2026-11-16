export async function GET(req: Request, { params }: { params: { id: string } }) {
  return Response.json(await sql`${sql.unsafe(`DELETE FROM t WHERE id = ${params.id}`)}`);
}
