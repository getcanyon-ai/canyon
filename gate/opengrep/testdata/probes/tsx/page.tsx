export default function Page({ searchParams }: { searchParams: { note: string } }) {
  return <div dangerouslySetInnerHTML={{ __html: searchParams.note }} />;
}
